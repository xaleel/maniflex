# Custom Endpoints (Actions)

The five generated REST routes per model cover the standard CRUD shape, but
some endpoints don't fit that shape — `POST /orders/{id}/cancel`,
`POST /invoices/{id}/send`, `GET /reports/revenue`. *Actions* are maniflex's
mechanism for adding these.

## Registering an action

An action is a method, a path, and a handler:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/{id}/cancel",
    Handler: cancelOrder,
})
```

The handler receives the standard `*maniflex.ServerContext`:

```go
func cancelOrder(ctx *maniflex.ServerContext) error {
    orderID := ctx.URLParam("id")

    if _, err := ctx.GetModel("Order").Update(orderID, map[string]any{
        "status": "cancelled",
    }); err != nil {
        return err
    }

    ctx.Response = &maniflex.APIResponse{
        StatusCode: http.StatusOK,
        Data:       map[string]any{"ok": true},
    }
    return nil
}
```

## The trimmed pipeline

Action requests run a shorter pipeline than CRUD requests:

```
Auth → [per-action middleware...] → handler → Response
```

`Deserialize`, `Validate`, `Service`, and `DB` are skipped. The action handler
is responsible for parsing its own body (`ctx.BindJSON`) and performing its
own database work (via `ctx.GetModel`, `ctx.RawExec`, or directly).

`ctx.Operation` is `OpAction` inside the handler. Middleware registered on the
trimmed-out steps with `ForOperation(maniflex.OpAction)` does not run; only `Auth`
and `Response` middleware do.

> **DB-step middlewares don't cover actions.** Anything registered on
> `Pipeline.DB` — including `db.RateLimit`, `db.AuditLog`, `db.ForceFilter`, and
> `db.Tenancy` — is silently skipped for action routes. For an all-action service
> this means zero rate limiting, zero audit records, and **no tenancy** unless you
> wire them per action. Use the action-flavoured variants in the action's own
> `Middleware` list: `db.RateLimitAction(cfg)` (keys on the caller + method/path),
> `db.AuditLogAction(sink)` (records actor/resource/result from the action
> context), and `db.TenancyAction(field, fn)` / `db.ForceFilterAction(field, fn)`
> (row-level scoping — see
> [Scoping Actions](../middleware-catalogue/db.md#scoping-actions-tenancyaction--forcefilteraction)).

Under `db.TenancyAction` / `db.ForceFilterAction` the handler's database work is
scoped through `ctx.GetModel`, the typed generics, `ctx.Aggregate`,
`ctx.LockForUpdate`, and the `Tx` from `ctx.BeginTx` (so `WithTransaction` and
`Batch` work too) — and `ctx.RawQuery`, `ctx.RawExec`, `ctx.Search` and
`ctx.RecursiveQuery` **refuse**, because a scope cannot be applied to raw SQL and
running it anyway would return every tenant's rows. `ctx.Unscoped()` bypasses
that deliberately where a path genuinely must. An action with no scope registered
is unaffected: every path behaves as it always has.

## Per-action middleware

Actions can carry their own middleware list, which runs between `Auth` and the
handler:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/{id}/cancel",
    Handler: cancelOrder,
    Middleware: []maniflex.MiddlewareFunc{
        auth.RequireRole("admin"),
        idempotency.Key("Idempotency-Key"),
    },
})
```

This is the equivalent of the Service step for an action — anything that
should run before the handler but after authentication.

## Reading input

The action handler does its own request parsing:

```go
type RefundReq struct {
    Amount float64 `json:"amount"`
    Reason string  `json:"reason"`
}

func refundOrder(ctx *maniflex.ServerContext) error {
    var req RefundReq
    if err := ctx.BindJSON(&req); err != nil {
        return nil  // ctx.Abort already called
    }

    // ... work ...

    ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
    return nil
}
```

`ctx.BindJSON` enforces the same 4 MB body limit as the default Deserialize
step. `ctx.URLParam` and `ctx.QueryParam` read URL and query parameters.

> **Multipart uploads:** `ctx.Files` is populated by the Deserialize step, which
> actions skip — so it is **always empty** inside an action. To accept a file
> upload in an action, parse the request yourself:
>
> ```go
> if err := ctx.Request.ParseMultipartForm(32 << 20); err != nil {
>     ctx.Abort(http.StatusBadRequest, "BAD_REQUEST", "invalid multipart form")
>     return nil
> }
> for _, headers := range ctx.Request.MultipartForm.File {
>     // headers[0].Open() → the uploaded file
> }
> ```

## Transactional actions

`ctx.BeginTx` works inside an action just as it does in middleware. For most
actions, wrap the handler body in a `BeginTx` / `Commit` block:

```go
func cancelOrder(ctx *maniflex.ServerContext) error {
    tx, err := ctx.BeginTx(ctx.Ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()
    ctx.Tx = tx

    // ... transactional work via ctx.GetModel / ctx.RawExec ...

    return tx.Commit()
}
```

Because the action does not pass through the Service step, `maniflex.WithTransaction`
registered there does not apply — actions manage their own transactions.

> **SQLite deadlock — fetch the model accessor _after_ setting `ctx.Tx`.** The
> SQLite adapter uses a single write connection (`MaxOpenConns(1)`). A
> `ctx.GetModel(...)` accessor binds to whatever `ctx.Tx` is at the time you
> call `GetModel`. If you grab the accessor *before* `BeginTx` and then write
> with it after `ctx.Tx = tx`, the write opens a *second* writer connection and
> blocks forever behind the transaction holding the single writer — the request
> hangs with no error. Always call `ctx.GetModel(...)` **after** `ctx.Tx = tx`:
>
> ```go
> tx, _ := ctx.BeginTx(ctx.Ctx, nil)
> defer tx.Rollback()
> ctx.Tx = tx
> orders := ctx.GetModel("Order") // bound to ctx.Tx now — safe
> orders.Update(id, patch)
> if err := tx.Commit(); err != nil { return err }
> ctx.Tx = nil // reset: reads built for the response after commit must not
>              // route through the finished tx
> ```

## Streaming a raw (non-JSON) response

An action normally returns JSON by setting `ctx.Response`. For a binary or
streaming endpoint — serving an image, a generated PDF, a CSV export — write
directly to `ctx.Writer` (the raw `http.ResponseWriter`) and **leave
`ctx.Response` nil**. After the handler returns, the framework writes nothing
further when `ctx.Response` is nil, so the bytes you wrote are the whole
response:

```go
func downloadEvidence(ctx *maniflex.ServerContext) error {
    f, err := openEvidence(ctx.URLParam("id"))
    if err != nil {
        ctx.Abort(http.StatusNotFound, "NOT_FOUND", "evidence not found")
        return nil
    }
    defer f.Close()

    ctx.Writer.Header().Set("Content-Type", "image/png")
    ctx.Writer.WriteHeader(http.StatusOK)
    _, err = io.Copy(ctx.Writer, f) // leave ctx.Response nil
    return err
}
```

When the bytes are an `mfx:"file"` model field, prefer the built-in per-model
attachment route (`GET /{model}/{id}/{field}`) instead — it runs the read
pipeline (auth, soft-delete, tenancy) and streams for you. If you scope a
`db.ForceFilter` to `OpList`/`OpRead`, remember to add `OpReadAttachment` too, or
downloads bypass the filter.

## Documenting an action in OpenAPI

Actions appear in the generated [OpenAPI spec](../using-the-api/openapi.md) automatically. By
default each one contributes its method, path (with path parameters extracted
from `{...}` segments), `Summary`, `Tags`, and `Deprecated` flag:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/{id}/cancel",
    Summary: "Cancel an order",
    Tags:    []string{"Orders"},
    Handler: cancelOrder,
})
```

For request/response bodies, query parameters, and security, fill in the
optional `OpenAPI` block. Its most useful feature is **schema inference**: point
`RequestSchema` / `ResponseSchema` at a Go struct tagged with the same `json`
and `mfx` tags you already use on models, and maniflex reflects it into a JSON
schema — no hand-written OpenAPI types:

```go
type RescheduleReq struct {
    NewTime string `json:"new_time" mfx:"required"`
    Reason  string `json:"reason"`
}

type RescheduleResp struct {
    ID     string `json:"id"`
    Status string `json:"status" mfx:"enum:scheduled|cancelled"`
}

server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/appointments/{id}/reschedule",
    Summary: "Reschedule an appointment",
    Handler: reschedule,
    OpenAPI: maniflex.ActionOpenAPI{
        Description:    "Moves an appointment to a new time.",
        RequestSchema:  RescheduleReq{},
        ResponseSchema: RescheduleResp{},
        ResponseStatus: http.StatusOK, // status the response schema documents; defaults to 200
        QueryParams: []maniflex.OASParameter{{
            Name: "notify", In: "query",
            Schema: &maniflex.OASSchema{Type: "boolean"},
        }},
        Security: []map[string][]string{{"bearerAuth": {}}},
    },
})
```

The reflected schemas honour the field tags you already use on models —
`required`, `enum`, `min`, `max`, `readonly`, `writeonly` — and skip `hidden`
fields. `RequestSchema` and `ResponseSchema` each accept a struct value, a
pointer, or a `reflect.Type`.

`Security` names a scheme you register separately with
[`openapi.AddSecurityScheme`](../middleware-catalogue/openapi.md).

If you'd rather build the OpenAPI types by hand, set `RequestBody` and
`Responses` directly on the `ActionConfig` — those take precedence over the
inferred schemas when both are present.

## Serving a model's own path from an action

An action and a model cannot both own the same method + path. Registering an
action at a path a model already owns (e.g. `GET /threads` when a model's table
is `threads`) is rejected **at startup** with a clear panic, rather than letting
the router silently mount two handlers and serve whichever one it happens to
match first:

```text
panic: maniflex: action GET /threads would shadow the auto-generated collection route for model "Thread"
```

The check covers every route a model mounts, not just the five CRUD ones: the
`export` and `aggregate` endpoints, per-field attachment paths
(`GET /{model}/{id}/{field}`), and a singleton's `GET` / `PATCH` on its bare path.
A conflict is reported when the action resolves the same path **and** method as a
model route — a path parameter's name is irrelevant (`GET /threads/{threadId}`
collides with the read route just as `GET /threads/{id}` does), while a method the
model does not serve at that path is free to take (`POST /threads/{id}` is fine —
the item route has no `POST`).

When you want to serve a model's collection path yourself — returning a custom
shape, or composing several models — mark the model **headless** so it mounts no
REST routes at all, freeing its path for the action:

```go
server.MustRegister(Thread{}, maniflex.ModelConfig{Headless: true})

server.Action(maniflex.ActionConfig{
    Method:  "GET",
    Path:    "/threads",     // no collision: Thread mounts no routes
    Handler: listThreads,
})
```

A headless model is still registered in full — it migrates, participates in
relations, and is reachable through `ctx.GetModel("Thread")` and typed CRUD — it
simply has no auto-generated HTTP surface (and no auto-generated OpenAPI paths;
its schema is still emitted for `$ref`s). Use it whenever the generated CRUD
shape doesn't match the contract you want to expose for that resource.

## When to use an action

| Need | Use |
|---|---|
| Standard CRUD | The generated routes |
| One-off state transitions (`/cancel`, `/publish`) | Action |
| Aggregations and reports | Action, or [Raw Queries & Query Models](raw-queries.md) |
| Bulk operations | [Batch Operations & Sagas](batch-saga.md) |
| Background processing | [Events & Background Jobs](events-jobs.md) |

Reserve actions for endpoints that genuinely don't fit CRUD. Resist the
temptation to use them as a general-purpose handler API — the framework's
strength is in the generated routes; every action is one more thing to test
and document by hand.
