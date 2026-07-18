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

### The `maniflex.Batch` helper

The hand-rolled `BeginTx` / `ctx.Tx` / `defer Rollback` dance above is
canonicalised by `maniflex.Batch(ctx, func(*maniflex.Batcher) error)`
(`batch.go`). It opens a transaction (or joins `ctx.Tx` if one is already set),
points `ctx.Tx` / `ctx.Ctx` at it for the duration of the callback, commits on
success, and rolls back on any returned error or `ctx.Abort`. The `*Batcher`
exposes the same five CRUD operations, all enlisted in the shared transaction:

```go
err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
    inv, err := b.Create("Invoice", invoiceData)
    if err != nil {
        return err
    }
    for _, line := range lines {
        line["invoice_id"] = inv["id"]
        if _, err := b.Create("InvoiceLine", line); err != nil {
            return err // rolls the whole batch back
        }
    }
    return nil
})
```

Prefer `maniflex.Batch` over the manual transaction plumbing shown above — it
gets the rollback, abort, and `ctx.Tx` restoration semantics right. A single
batch transaction cannot span adapters; use a saga for cross-database work.

For larger imports, batch the inserts (`INSERT … VALUES (…), (…), …` via
`ctx.RawExec`) and commit every N rows.

## Cross-service workflows: sagas

When a workflow touches more than one downstream — charge a payment provider,
reserve inventory, notify a partner — a single database transaction is no
longer enough. The standard pattern is a **saga**: a sequence of forward
steps, each with a compensating undo step.

maniflex ships a lightweight saga coordinator in `pkg/saga`:
`saga.New(name).Step(name, do, undo).Execute(ctx, state)` runs the forward
steps in order and, on any failure, runs the compensating `undo` functions in
reverse order.

```go
err := saga.New("dispense_charge").
    Step("create_invoice",  createInvoice, voidInvoice).
    Step("debit_ar_ledger", debitAR,       reverseAR).
    Step("deduct_stock",    deductStock,   restock).
    Execute(ctx, saga.State{"patient_id": pid, "items": items})
```

It is an **in-process, non-crash-safe** coordinator: if the process dies
mid-saga, no compensation runs. For durable, crash-safe workflows the
transactional outbox pattern below remains the option to reach for (or wrap the
saga's `Execute` inside a `pkg/jobs` job so a retry re-drives it). The outbox
mechanics fit naturally on the pipeline:

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
- [Transactions](../the-request-pipeline/transactions.md) — `maniflex.WithTransaction`, manual
  `BeginTx`, and `LockForUpdate`.
- [Custom Endpoints (Actions)](actions.md) — the right place to host a bulk
  endpoint.
