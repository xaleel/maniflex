# ServerContext

`*maniflex.ServerContext` is the object threaded through every pipeline step for one
HTTP request. Steps read from it, write to it, and call `next()` to proceed.
Middleware does the same. This page documents the fields and methods middleware
will commonly touch.

## Lifecycle

A new `ServerContext` is constructed by the handler for every request, populated
incrementally by the pipeline, and discarded once the response is written. It
is **not** safe to share across requests or goroutines.

The fields populated by each step are:

| Step | Sets |
|---|---|
| handler (before Auth) | `Request`, `Writer`, `Ctx`, `Model`, `Operation`, `ResourceID`, `RequestID`, `TraceID` |
| Auth | `Auth` (when user middleware populates it) |
| Deserialize | `RawBody`, `ParsedBody`, `Query`, `Files` |
| Service | (whatever user middleware sets) |
| DB | `DBResult`, possibly `Tx` |
| Response | `Response`, then writes it to `Writer` |

## Routing context

Set by the handler before Auth runs; safe to read in any step.

| Field | Meaning |
|---|---|
| `Request` | the original `*http.Request` |
| `Writer` | the underlying `http.ResponseWriter` |
| `Ctx` | the request `context.Context`; cancellation propagates from here |
| `Model` | the `*ModelMeta` for the resource — name, table, fields, relations |
| `Operation` | the `Operation` being performed (`OpCreate`, `OpList`, …) |
| `ResourceID` | the `{id}` path parameter, empty for list and create |
| `RequestID` | chi's request ID, echoed in `X-Request-Id` |
| `TraceID` | the W3C `traceparent` header, when present |

## Step outputs

Populated in order by the pipeline.

| Field | Populated by | Type |
|---|---|---|
| `RawBody` | Deserialize | `[]byte` — the raw request bytes |
| `ParsedBody` | Deserialize | `*RequestBody` — read-only JSON-keyed body (mutate via `SetField`) |
| `Record` | Deserialize | the typed record carrier (`*T` for `ctx.Model`) bound from the body |
| `Query` | Deserialize | `*QueryParams` — pagination, filters, sorts, includes |
| `Files` | Deserialize (multipart only) | `map[string]*UploadedFile` |
| `DBResult` | DB | `*ListResult` for lists; the record otherwise (a typed `*T` on reads) |
| `Response` | Response | `*APIResponse` — the envelope written to the wire |

Setting `Response` from any step causes the remaining steps to skip and the
prepared envelope to be written. See `Abort` below.

## Auth

`Auth *AuthInfo` is populated by Auth middleware. When `nil`, the request is
anonymous.

```go
type AuthInfo struct {
    UserID       string
    Roles        []string
    Claims       map[string]any
    TenantID     string
    IdentityType AuthIdentityType  // human, service_account, anonymous
    Scopes       []string
    SessionID    string
    AuthMethod   string             // "jwt", "api_key", "session", …
}
```

`ctx.HasRole(role string) bool` is a convenience wrapper that returns false
when `Auth` is `nil`.

## Transactions

`Tx Tx` carries the active transaction, if any. When set, the default DB step
routes through it. `ctx.BeginTx(ctx.Ctx, opts)` returns a `Tx` and is the
standard way for middleware to start one. See [Transactions](transactions.md)
for the full pattern.

## Aborting the pipeline

`ctx.Abort(status int, code, message string)` populates `ctx.Response` with an
error envelope. The current middleware must then return `nil` without calling
`next()`. Subsequent steps are skipped; the Response step writes the prepared
error.

```go
if header == "" {
    ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
    return nil
}
```

### Calling `next()` after `Abort`

`Abort` does not stop the pipeline — it only populates `ctx.Response`. If the
middleware calls `next()` afterwards, the chain continues exactly as if the
abort had not happened:

- The remaining steps still execute, with all their side effects. The DB step
  will still issue its query and possibly modify the database; a Service
  middleware will still call out to external services.
- Any of those steps may **overwrite** `ctx.Response` — for example, the DB
  step replaces it with a `404 NOT_FOUND` if the record is missing, or the
  default Response step builds a `200 OK` envelope from `ctx.DBResult`.
  Whichever step writes last wins.
- If nothing downstream touches `ctx.Response`, the original abort envelope is
  preserved and sent to the client — but the side effects have already
  happened.

The result is almost always a bug: either the client sees a misleading status
(a write succeeded but the response claims it was rejected), or the database
is mutated by a request that was meant to be refused. Always return without
`next()` after `Abort`.

## Reading input

Three helpers wrap common request reads:

| Method | Purpose |
|---|---|
| `BindJSON(v any) error` | decode the body into `v`, enforcing the 4 MB limit |
| `EnsureRawBody() ([]byte, error)` | return `RawBody`, reading/buffering/restoring the body if an earlier step hasn't (for middleware that needs the raw bytes in a trimmed action/search pipeline) |
| `URLParam(name string) string` | read a chi URL parameter |
| `QueryParam(name string) string` | read a URL query parameter |

`BindJSON` calls `Abort` internally on error and returns a non-nil error so the
caller can `return nil` immediately.

## The request body

`ctx.ParsedBody` holds the deserialized JSON (or multipart form) body as a
`*RequestBody`. It is **read-only**: there is no exported way to index or assign
it, so a stray `ctx.ParsedBody["x"] = y` is a compile error. This is deliberate.
The body is mirrored onto a typed record (`ctx.Record`), and the only mutators —
`ctx.SetField` / `ctx.DeleteField` — keep both in sync; writing the map directly
would update one and not the other, and the change could be silently dropped at
the DB step.

### Reading

| Call | Returns |
|---|---|
| `ctx.Field(name string) (any, bool)` | one field by its JSON name |
| `ctx.ParsedBody.Has(name) bool` | whether a key is present (an explicit `null` counts) |
| `ctx.ParsedBody.Keys() []string` / `.Len() int` | the top-level key set |
| `ctx.ParsedBody.Map() map[string]any` | a **copy** of the body, for read-only consumers |

All are nil-safe: `ctx.ParsedBody` is `nil` for body-less requests (GET, DELETE)
and the readers return zero values rather than panicking.

For typed access, read the whole body as the concrete model struct:

```go
u, ok := maniflex.For[User](ctx)   // (*User, bool) — false if no User body is bound
u, err := maniflex.Bind[User](ctx) // (*User, error) — errors when absent

// or adapt a typed handler straight into middleware:
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

### Writing

Middleware that injects or rewrites a field must go through these setters so the
value reaches both the body and the typed record (and so the DB step persists it):

| Call | Effect |
|---|---|
| `ctx.SetField(name string, value any)` | set a field by its JSON name |
| `ctx.DeleteField(name string)` | remove a field (e.g. strip an input-only field) |

```go
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    ctx.SetField("owner_id", ctx.Auth.UserID) // force the owner server-side
    return next()
}, maniflex.ForOperation(maniflex.OpCreate))
```

## Cross-step storage

For state that one middleware needs to pass to another:

```go
ctx.Set("invoiceID", inv.ID)
// later, in another middleware:
id, ok := ctx.Get("invoiceID")
```

The store is per-request and discarded with the context.

## Direct database access

Middleware that needs to reach beyond `ctx.Model` — to read another model, run
a raw query, or take a row lock — has four entry points, all routed through
`ctx.Tx` when one is active:

| Method | Purpose |
|---|---|
| `GetModel(name string) *ModelAccessor` | CRUD on any registered model (`.List` / `.Read` / `.Create` / `.Update` / `.Delete`) |
| `RawQuery(sql string, args ...any) ([]map[string]any, error)` | parameterised `SELECT`, CTE-`SELECT`, or a data-modifying statement with `RETURNING` (e.g. `UPDATE … RETURNING id`) |
| `RawExec(sql string, args ...any) (int64, error)` | parameterised non-`SELECT` (returns rows affected) |
| `LockForUpdate(modelName, id string) (map[string]any, error)` | pessimistic row lock; requires `ctx.Tx` |

`GetModel` returns an accessor whose methods route through `ctx.Tx` when set,
so middleware in a transaction does not have to thread the `Tx` manually.

### Typed cross-model helpers

`GetModel(name)` is dynamic — string-named, exchanging `map[string]any`. For
compile-time types use the generic free functions, which resolve the model from
the type parameter and route through `ctx.Tx` the same way (so they also
participate in `maniflex.Batch`):

```go
u, err   := maniflex.Read[User](ctx, id)        // *User
all, err := maniflex.List[User](ctx, nil)        // []*User
created, err := maniflex.Create(ctx, &User{Name: "Jane"})
maniflex.Update(ctx, id, &User{ /* full record */ })
maniflex.Delete[User](ctx, id)
```

Results that are **not** a registered model — raw SQL, aggregates, recursive
queries — use `maniflex.Row` (an alias for `map[string]any`); `RawQuery`,
`Aggregate`, and `RecursiveQuery` return `[]maniflex.Row`.

Placeholders in raw SQL are rebound to the adapter's dialect, so `?` works on
both SQLite and Postgres (`$N`). Always pass values as `args` — never interpolate
them into the query string.

## Logging

`ctx.Logger() *slog.Logger` returns a `slog` logger pre-seeded with
`request_id`, `trace_id`, and `service` attributes, so log lines emitted from
middleware are correlated automatically.

```go
ctx.Logger().Info("payment captured",
    slog.String("invoice_id", inv.ID),
    slog.Float64("amount", inv.Total),
)
```

## Service name

`ctx.ServiceName()` returns the `Config.ServiceName` configured on the server.
Middleware uses this to enrich audit records or outgoing requests without
holding a reference to the framework `Config`.

## Next

- **[Writing Middleware](middleware.md)** — composing middleware on these fields.
- **[Transactions](transactions.md)** — `ctx.Tx`, `BeginTx`, `LockForUpdate`.
- **[Error Handling](errors.md)** — `Abort` and the response envelope.
