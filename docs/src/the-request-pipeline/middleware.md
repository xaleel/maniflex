# Writing Middleware

A middleware is a function that runs as part of one of the six pipeline steps.
It can inspect and modify the request context, call into the database, decide
whether to proceed, and inject behaviour before or after the step's default
handler.

## Signature

```go
type MiddlewareFunc func(ctx *maniflex.ServerContext, next func() error) error
```

A middleware does one of two things:

- **Continue the pipeline** — perform its work, then call `next()` and return
  the result. The chain executes the remaining middleware in the step and then
  the steps after it.
- **Short-circuit** — set `ctx.Response` (typically via `ctx.Abort(...)`) and
  return `nil` _without_ calling `next()`. The remaining steps are skipped and
  the prepared response is written to the wire.

```go
func bearerToken(ctx *maniflex.ServerContext, next func() error) error {
    header := ctx.Request.Header.Get("Authorization")
    if !strings.HasPrefix(header, "Bearer ") {
        ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
        return nil
    }
    ctx.Auth = &maniflex.AuthInfo{UserID: parseSubject(header)}
    return next()
}
```

## Typed middleware

For middleware that works with the request body, `maniflex.Handle[T]` adapts a
typed handler into a `MiddlewareFunc`. It hands you the bound record as a
concrete `*T` (the same value `ctx.Record` holds) instead of a map, runs only
when a `*T` body is bound, and is skipped on body-less operations:

```go
server.Pipeline.Service.Register(
    maniflex.Handle(func(ctx *maniflex.ServerContext, u *User) error {
        if u.Age < 18 {
            ctx.Abort(http.StatusUnprocessableEntity, "TOO_YOUNG", "must be 18+")
        }
        return nil
    }),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
)
```

To change a field, call `ctx.SetField(name, value)` rather than mutating the
struct, so the write reaches both the body and the record (see
[ServerContext › The request body](context.md)). For an ad-hoc typed read inside
a plain middleware, use `maniflex.For[T](ctx)` / `maniflex.Bind[T](ctx)`.

## Registration

Each pipeline step exposes a `*StepRegistry` on `server.Pipeline`. Register a
middleware on the step where its work belongs:

```go
server.Pipeline.Auth.Register(bearerToken)
```

Without options, the middleware applies to every model and every operation.

`Register` must be called **before** `Start()` or `Handler()`. Building the router
closes the registration window: each step's chain is composed once per
(model, operation) and cached from then on, rather than rebuilt on every request,
so the middleware set has to stop changing. A `Register` after that point panics —
it could otherwise only apply to some requests and not others, and it would be
mutating a slice live requests are reading.

```go
server.Pipeline.Auth.Register(bearerToken)   // fine
server.Start()                               // window closes
server.Pipeline.DB.Register(audit)           // panics
```

## Scoping

Two functional options narrow the scope. They are independent and may be
combined.

### `ForModel(names ...string)`

Restrict to one or more models, by struct name:

```go
server.Pipeline.Service.Register(hashPassword, maniflex.ForModel("User"))
```

### `ForOperation(ops ...Operation)`

Restrict to specific operations:

```go
server.Pipeline.Auth.Register(requireToken,
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

Operation values: `OpList`, `OpRead`, `OpCreate`, `OpUpdate`, `OpDelete`,
`OpOptions`, `OpAction`. Registering on Validate/Service/DB with `OpAction` has
no effect — those steps are skipped for action endpoints. A `HEAD` request runs
as the `GET` it mirrors (`OpRead` / `OpList`), so scope it with those.

### Combining

```go
server.Pipeline.Service.Register(chargePayment,
    maniflex.ForModel("Order"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

## Position

By default, a middleware runs **before** the step's default handler. Use
`AtPosition` to change that.

| Position               | When the middleware runs                                                  |
| ---------------------- | ------------------------------------------------------------------------- |
| `maniflex.Before` (default) | before the default handler                                                |
| `maniflex.After`            | after the default handler                                                 |
| `maniflex.Replace`          | instead of the default handler — the step's built-in behaviour is skipped |

```go
// Run after the DB step succeeds — useful for audit logs.
server.Pipeline.DB.Register(auditLog,
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)

// Replace the default DB step for one model entirely.
server.Pipeline.DB.Register(customDispatch,
    maniflex.ForModel("LegacyOrder"),
    maniflex.AtPosition(maniflex.Replace),
)
```

Within a step, all matching **Before** middlewares run in registration order,
then the core handler (default or `Replace`), then all matching **After**
middlewares in registration order. If multiple `Replace` middlewares match,
the last one registered wins.

A `Replace` on the **DB step** takes over feeding the Response step, so it must
leave `ctx.DBResult` in the shape that step expects: a `*maniflex.ListResult` for
a list, and a record (`map[string]any` or a `*T`) for a read, create, or update.
Anything else is rejected with `500 INVALID_DB_RESULT` naming the type it got. On
a `ListResult` you need only set `Items` and `Total` — a missing or partial
`Query` is filled in with the default page and limit.

## Naming for traces

`maniflex.WithName("name")` attaches a human label to a middleware for use in
pipeline trace logs (enabled via `Config.Trace`). It does not change runtime
behaviour:

```go
server.Pipeline.Auth.Register(rateLimit, maniflex.WithName("rate-limiter"))
```

## Step-specific guidance

| Step            | What middleware here typically does                                                                                |
| --------------- | ------------------------------------------------------------------------------------------------------------------ |
| **Auth**        | Verify a token, populate `ctx.Auth`, reject unauthenticated requests.                                              |
| **Deserialize** | Rarely customised. `After` middleware can rewrite the body via `ctx.SetField` / `ctx.DeleteField`.                  |
| **Validate**    | Custom validation that goes beyond `mfx:` tags. Abort with 422 on failure.                                         |
| **Service**     | Business logic — derive fields, call external services, start transactions (`maniflex.WithTransaction`).                |
| **DB**          | Hooks around the database call. `After` middleware sees `ctx.DBResult`; `Replace` substitutes a different backend. |
| **Response**    | `After` middleware can add headers; `Replace` lets you write a non-envelope response.                              |

## After-middleware error handling

An `After` middleware sees `ctx.Response` when the default step has populated
it. Inspect it to decide whether to act:

```go
func auditLog(ctx *maniflex.ServerContext, next func() error) error {
    if err := next(); err != nil {
        return err
    }
    // don't audit failed writes
    if ctx.Response != nil && ctx.Response.StatusCode < 400 {
        record(ctx)
    }
    return nil
}
```

## Per-model middleware at registration

For middleware that belongs to exactly one model, `ModelConfig.Middleware`
attaches hooks scoped to that model at registration time, avoiding the separate
`Register` call:

```go
server.MustRegister(
    Order{}, maniflex.ModelConfig{
        Middleware: &maniflex.ModelMiddleware{
            Validate: []maniflex.MiddlewareFunc{checkStock},
            Service:  []maniflex.MiddlewareFunc{chargePayment},
        },
    },
)
```

Both forms are equivalent; choose whichever keeps the declaration close to the
code that depends on it.

## Built-in middleware

Several middleware functions ship with the framework or its satellite modules
— JWT auth, password hashing, audit logging, CORS, and more. They are
documented in [Middleware Catalogue](../middleware-catalogue/index.md).

## Next

- **[ServerContext](context.md)** — the fields a middleware reads and writes.
- **[Transactions](transactions.md)** — `maniflex.WithTransaction` as a Service-step
  middleware.
- **[Error Handling](errors.md)** — what `Abort` produces and how it propagates.
