# Transactions

A transaction wraps one or more database operations so they either all commit
or all roll back. In maniflex the unit of transactional work is normally a single
request: every database call performed by its pipeline runs in the same
transaction, and the transaction commits if and only if the request produces a
successful response.

## Enabling transactions

The shipped middleware `maniflex.WithTransaction` wraps the DB step in a
transaction. Register it on the Service step, scoped to the operations that
should be transactional:

```go
server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil), // nil = default isolation level
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

Once registered, every matching request executes the DB step (and any
subsequent After-DB middleware) inside a transaction. The middleware:

1. Begins a transaction before calling `next()`.
2. Stores it on `ctx.Tx` and the underlying `ctx.Ctx`, so all downstream code
   can join it.
3. Commits if `next()` returns nil and `ctx.Response` is a 2xx.
4. Rolls back if `next()` returns an error, if `ctx.Response.StatusCode >= 400`,
   or if anything panics.

The same middleware can be registered on the DB step at `Replace` position if
you want to substitute the default DB step entirely.

## Customising isolation

`maniflex.TxOptions` is an alias for `sql.TxOptions`:

```go
server.Pipeline.Service.Register(
    maniflex.WithTransaction(&maniflex.TxOptions{
        Isolation: sql.LevelSerializable,
        ReadOnly:  false,
    }),
    maniflex.ForModel("Invoice"),
)
```

SQLite ignores most isolation levels; for guaranteed write-locking on SQLite,
use `BEGIN IMMEDIATE` via the `_txlock=immediate` DSN option when opening the
database.

## Joining the transaction from middleware

When `ctx.Tx` is set, every CRUD call made through `ctx.GetModel(...)`,
`ctx.RawQuery`, `ctx.RawExec`, and the default DB step routes through it
automatically:

```go
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    // This Update participates in the same transaction as the DB step that follows.
    if _, err := ctx.GetModel("Inventory").Update(itemID, map[string]any{
        "reserved": true,
    }); err != nil {
        return err  // triggers rollback
    }
    return next()
}, maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate))
```

There is no separate "transactional" API — calling `ctx.GetModel` is enough.

## Starting a transaction manually

When `WithTransaction` does not fit — for example, when only part of a request
should be transactional, or when the transaction must span an action endpoint
— begin one yourself:

```go
tx, err := ctx.BeginTx(ctx.Ctx, nil)
if err != nil {
    return err
}
ctx.Tx = tx
defer tx.Rollback()  // no-op after Commit

// ... transactional work via ctx.GetModel / ctx.RawExec ...

if err := tx.Commit(); err != nil {
    return err
}
ctx.Tx = nil  // clear so post-commit code uses the bare adapter
```

`tx.Rollback()` after a successful commit is safe — it returns
`sql.ErrTxDone`, which the framework swallows. Always defer it.

## Pessimistic locking

Inside a transaction, `ctx.LockForUpdate(modelName, id)` acquires a row-level
write lock and returns the current row:

```go
row, err := ctx.LockForUpdate("StockBalance", stockID)
if err != nil {
    return err
}
if row["quantity"].(int64) < 1 {
    ctx.Abort(http.StatusConflict, "OUT_OF_STOCK", "no inventory remaining")
    return nil
}
```

On Postgres this appends `FOR UPDATE` to the SELECT. On SQLite the lock is at
the transaction level — the row is protected because the transaction itself is
write-locked.

`LockForUpdate` returns an error if `ctx.Tx` is nil; the lock is meaningless
outside a transaction.

## Joining from outside a ServerContext

Code without access to `*ServerContext` — for example, a job-queue helper, or an
outbox writer — can retrieve the active transaction from `ctx.Ctx` using
`maniflex.TxFromContext`:

```go
func enqueue(ctx context.Context, job Job) error {
    if tx := maniflex.TxFromContext(ctx); tx != nil {
        return enqueueInTx(tx, job)
    }
    return enqueueDirect(job)
}
```

`WithTransaction` stores the active transaction on the `context.Context` for
exactly this purpose.

The transaction is reachable only while it is open. Once `WithTransaction`
commits or rolls back, both `ctx.Tx` and `TxFromContext` go back to `nil`, so a
middleware that resumes after its `next()` — an audit hook, an outbox enqueue —
takes the `tx == nil` branch and writes through the bare adapter, rather than
into a finished transaction.

## Adapter scope

A transaction lives on a single `DBAdapter`. `ctx.BeginTx` opens the
transaction on the request's model adapter (its `ModelConfig.Adapter` if
set, otherwise `Config.DB`). All operations on that `ctx.Tx` must target
models routed to the same adapter.

`maniflex.Batch` enforces this at runtime: calling `b.Create("X", ...)` for a
model that routes to a different adapter than the batch transaction
returns an error suggesting `pkg/saga` for cross-adapter coordination.
See [Per-model adapter routing](../deployment/databases.md#per-model-adapter-routing).

## Nesting

`WithTransaction` is idempotent: if `ctx.Tx` is already set when it runs, it
simply calls `next()` without starting a new transaction. The outer transaction
remains the unit of commit.

SQLite does not support nested transactions; calling `ctx.BeginTx` while one
is already active returns an error.

## Failure semantics

A transaction is rolled back when:

- the chain returns a non-nil error from any step;
- `ctx.Response` is set to a status `>= 400` (e.g. via `ctx.Abort`);
- a panic occurs (the framework's panic recoverer ensures rollback).

`WithTransaction` is committed when:

- the chain completes without error and `ctx.Response` is a 2xx (or unset).

A commit failure is reported as `500 TX_COMMIT_ERROR`; a begin failure as
`500 TX_BEGIN_ERROR`.

## Next

- **[Writing Middleware](middleware.md)** — how to register `WithTransaction`
  or write your own transactional middleware.
- **[Error Handling](errors.md)** — how a rollback surfaces to the client.
