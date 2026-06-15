# Batch Operations & Sagas

The generated REST routes work on one row at a time. When the work fans out
— inserting hundreds of rows from a CSV, fulfilling an order across inventory,
payment, and shipping — two patterns appear: **batch** for atomic same-table
work, and **saga** for multi-step workflows that span services.

## Batch inside a single transaction

The simplest "bulk write" is a single transaction that issues many inserts or
updates. Use an [action endpoint](actions.md) so the request does not pass
through the per-row Validate/Service hooks:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/users/import",
    Handler: importUsers,
})

func importUsers(ctx *maniflex.ServerContext) error {
    var req struct {
        Users []map[string]any `json:"users"`
    }
    if err := ctx.BindJSON(&req); err != nil {
        return nil
    }

    tx, err := ctx.BeginTx(ctx.Ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()
    ctx.Tx = tx

    users := ctx.GetModel("User")
    inserted := 0
    for _, row := range req.Users {
        if _, err := users.Create(row); err != nil {
            ctx.Abort(http.StatusConflict, "IMPORT_FAILED",
                fmt.Sprintf("row %d: %s", inserted, err.Error()))
            return nil
        }
        inserted++
    }

    if err := tx.Commit(); err != nil {
        return err
    }

    ctx.Response = &maniflex.APIResponse{
        StatusCode: http.StatusCreated,
        Data:       map[string]any{"inserted": inserted},
    }
    return nil
}
```

Either every row commits or none does. Validation still runs because
`ctx.GetModel(...).Create` goes through the adapter — but per-row middleware
on the Service or Validate steps does not, since this is an action.

For larger imports, batch the inserts (`INSERT … VALUES (…), (…), …` via
`ctx.RawExec`) and commit every N rows.

## Cross-service workflows: sagas

When a workflow touches more than one downstream — charge a payment provider,
reserve inventory, notify a partner — a single database transaction is no
longer enough. The standard pattern is a **saga**: a sequence of forward
steps, each with a compensating undo step.

maniflex does not impose a saga framework. The mechanics fit naturally on
the pipeline:

1. **Start a request transaction** with `maniflex.WithTransaction`. The local
   database changes commit or roll back atomically.
2. **Make external calls from the Service step**, recording an outbox row in
   the same transaction for each call that needs a follow-up.
3. **Process the outbox asynchronously** with a background runner (from
   `jobs/redis` or a similar) that performs the external call, marks the
   outbox row done, and triggers compensation on failure.

```go
server.Pipeline.Service.Register(maniflex.WithTransaction(nil),
    maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate))

server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if err := next(); err != nil {
        return err
    }
    // Both writes are in the same transaction as the Order insert.
    _, err := ctx.GetModel("OutboxEvent").Create(map[string]any{
        "kind":     "charge-payment",
        "payload":  ctx.DBResult,
        "status":   "pending",
    })
    return err
}, maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
```

A separate worker reads pending `OutboxEvent` rows and processes them. If the
payment provider fails, the worker records the failure and enqueues a
compensating action (e.g. "cancel the order").

This pattern — **transactional outbox + asynchronous worker** — is the
practical alternative to two-phase commit. It costs you one table and one
background job but gains durability and isolation that distributed
transactions cannot offer.

## When to use which

| Workload | Pattern |
|---|---|
| Bulk same-table writes | One transaction, one action endpoint |
| Multi-table writes touching only your database | `maniflex.WithTransaction` on the request |
| Writes that depend on external systems | Transactional outbox + saga |
| Long-running background work | Background job (see [Events & Background Jobs](events-jobs.md)) |

## See also

- [Events & Background Jobs](events-jobs.md) — running the outbox worker and
  emitting domain events.
- [Transactions](../transactions.md) — `maniflex.WithTransaction`, manual
  `BeginTx`, and `LockForUpdate`.
- [Custom Endpoints (Actions)](actions.md) — the right place to host a bulk
  endpoint.
