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
