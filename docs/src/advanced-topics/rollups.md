# Maintained Rollups

A rollup is a denormalised aggregate column on a parent that the framework keeps
in step with its children. `Order.PaidAmount` as `SUM(OrderPayment.amount)`,
`StoreSite.ReviewsCount` as `COUNT(Review)` — the columns an app would otherwise
recompute by hand in every write path, and which drift the moment one path
forgets.

Where [`ctx.Aggregate`](raw-queries.md#structured-aggregation-ctxaggregate)
computes an aggregate on **read**, a `Rollup` maintains one on **write**.

```go
srv := maniflex.New(cfg)
srv.MustRegister(Order{}, OrderPayment{})

// Every OrderPayment write recomputes its Order's paid_amount.
srv.MustRegisterRollup(maniflex.Rollup{
    Parent: "Order", ParentField: "paid_amount", Op: maniflex.AggSum,
    Child:  "OrderPayment", ChildField: "amount", On: "order_id",
})

// A rollup needs the child write to be transactional.
srv.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),
    maniflex.ForModel("OrderPayment"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

## Configuration

| Field | Meaning |
|---|---|
| `Parent` | model carrying the denormalised column |
| `ParentField` | JSON name of that column |
| `Op` | `AggSum`, `AggCount`, `AggAvg`, `AggMin`, or `AggMax` |
| `Child` | model whose rows are aggregated |
| `ChildField` | JSON name of the aggregated column (omit only for `AggCount`) |
| `On` | JSON name of the foreign key on `Child` pointing to `Parent`'s id |

Field names are resolved and **validated at registration** — a typo is a startup
error naming the field, not a silently drifted total. This is the whole reason
the config is a typed struct and not a `mfx:"rollup:sum(...)"` tag: a tag would
be a mini query-language inside a string, invisible to `go vet` and failing in
the worst possible way.

`RegisterRollup` returns an error; `MustRegisterRollup` panics. Both must be
called before `Start()`/`Handler()`.

## How it stays correct

On every create, update or delete of a child, the affected parent is
**recomputed from scratch** — `Op(ChildField)` over that parent's live children —
and written to the parent column, inside the child write's transaction.

Recomputing rather than applying a `+delta`/`-delta` is what makes it correct by
construction:

- **Delete** — the parent recomputes without the deleted row.
- **Re-parenting** — a PATCH that changes the child's foreign key recomputes
  **both** the old and the new parent.
- **Soft delete** — a soft-deleted child is excluded, matching what a fresh
  aggregate returns.
- **No drift** — the column is always exactly the aggregate of the rows it
  summarises, never an accumulator that can diverge.

Empty sets follow SQL: a sum or count of no children is `0`; a min/max/avg of no
children is `null`.

## Transactions are required

A rollup refuses a child write that is not in a transaction, with
`500 ROLLUP_NO_TX`. Without one, a child insert could commit while the parent
update fails — the exact drift the rollup exists to prevent. Register
`maniflex.WithTransaction` on the Service step for the child's writes, as above.
This follows the same fail-loud rule as `mfx:"lock_scope"`.

## Backfilling

Adding a rollup to a table that already has children, or reconciling a column
edited out of band, needs a one-time recompute:

```go
if err := srv.BackfillRollups(context.Background()); err != nil {
    log.Fatal(err)
}
```

`BackfillRollups` recomputes every registered rollup for every parent from the
current child rows. It reconciles rather than locks — each parent is recomputed
independently, and a concurrent live write is simply picked up by its own rollup
— so prefer a quiet window for very large tables.

## Cost and limits

- Each child write costs one extra aggregate query plus one parent update, inside
  the transaction. Concurrent children of the **same** parent serialise on that
  parent's row, which is what keeps the total consistent.
- The rollup fires on the generated CRUD routes and any write that runs the DB
  step. A write that bypasses the pipeline (a raw `INSERT`, a direct adapter
  call) does not trigger it — run `BackfillRollups` after such a bulk load.
- `AggCountDistinct` is not supported as a rollup op; write it by hand with an
  After-DB middleware if you need it.
