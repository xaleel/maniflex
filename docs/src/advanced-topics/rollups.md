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
| `Where` | optional `[]*FilterExpr` narrowing which children the aggregate covers |

Field names are resolved and **validated at registration** — a typo is a startup
error naming the field, not a silently drifted total. This is the whole reason
the config is a typed struct and not a `mfx:"rollup:sum(...)"` tag: a tag would
be a mini query-language inside a string, invisible to `go vet` and failing in
the worst possible way.

`RegisterRollup` returns an error; `MustRegisterRollup` panics. Both must be
called before `Start()`/`Handler()`.

## Filtering the children

`Where` narrows the rows the aggregate covers — the captured payments rather than
every payment row:

```go
srv.MustRegisterRollup(maniflex.Rollup{
    Parent: "Order", ParentField: "captured_amount", Op: maniflex.AggSum,
    Child:  "OrderPayment", ChildField: "amount", On: "order_id",
    Where: []*maniflex.FilterExpr{
        {Field: "status", Operator: maniflex.OpEq, Value: "captured"},
    },
})
```

The filters AND onto the foreign-key match and the soft-delete guard a rollup
always applies; filters sharing a `Group >= 1` OR among themselves, as
everywhere else. Fields take the DB column name or the json name and are
validated at registration alongside the rest of the config.

Narrowing costs nothing in correctness. Because every child write recomputes its
parent from scratch, a child that **leaves** the filtered set — a payment going
`captured` → `refunded` — moves the total exactly as a delete would, and one
that enters it is added. There is no delta to keep in step.

Only **flat filters on the child's own columns** are accepted. A nested-relation
filter (`author.status`) reads a column on a joined table and a locale filter
(`name.ar`) a key inside a JSON document; the recompute aggregates the child
table alone, so neither has anything to resolve against. Both are refused at
`RegisterRollup`, not at the first child write.

`BackfillRollups` deliberately does **not** apply `Where` when it discovers which
parents to visit. The filter says which children count, not which parents have
one: an order whose every payment fails the filter still has to be driven back to
`0`, and narrowing the discovery scan would skip it and leave the stale total in
place.

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

### Concurrent writes

Recomputing from scratch is only correct if the recompute sees every committed
sibling, so the parent's row is **locked before the aggregate runs**, not after.
Two concurrent child writes for the same parent therefore queue: the second waits
at the lock, and by the time it aggregates the first has committed and its row is
counted.

Locking after the aggregate — or not at all — would leave a window that loses
updates on Postgres under its default `READ COMMITTED`. Both transactions would
aggregate without seeing the other's uncommitted row, the second `UPDATE` would
block on the parent row and then overwrite the first with its own stale total,
and nothing would error. That drift is permanent, in exactly the counter a rollup
exists to keep correct.

A write that touches two parents — a re-parenting update recomputes both — takes
their locks in a fixed order, so two such writes moving children in opposite
directions wait for each other rather than deadlocking.

SQLite is unaffected either way and pays nothing for the lock: its write lock is
the transaction's own, taken at `BEGIN`, because `db/sqlite` opens write
connections with `_txlock=immediate`.

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

- Each child write costs one parent row lock, one aggregate query and one parent
  update, inside the transaction. Concurrent children of the **same** parent
  serialise on that parent's row from the lock until the transaction commits,
  which is what keeps the total consistent — so a very hot parent bounds how fast
  its children can be written.
- The rollup fires on the generated CRUD routes and any write that runs the DB
  step. A write that bypasses the pipeline (a raw `INSERT`, a direct adapter
  call) does not trigger it — run `BackfillRollups` after such a bulk load.
- A **cascade** does trigger it, though it does not run the DB step either. A
  child removed by its parent's `onDelete:cascade`, or re-pointed by
  `onDelete:setNull`, is taken out of its parent's total: the sweep notes every
  parent it disturbs and recomputes each one once, in a fixed order, when the
  walk ends. Registering a rollup is also what keeps that edge out of a database
  `ON DELETE` clause, which would remove the rows without telling anyone — see
  [Relations](../defining-your-api/relations.md#how-it-is-enforced-and-soft-delete).
- `AggCountDistinct` is not supported as a rollup op; write it by hand with an
  After-DB middleware if you need it.

## Rollups and row-level scopes

A recompute writes the **parent**, and the parent is whatever row the child's
foreign key names. Under a row-level scope (`db.Tenancy`, `db.ForceFilter`) that
matters: a child naming another tenant's parent would move a total in a row its
author can neither read nor reach.

The scope closes this at the write. Every `BelongsTo` key a create or update sets
is read through the request's own scope first, and a key pointing at a parent the
caller cannot see is refused with that parent's `404` — so the child never
exists, and there is nothing left for a later recompute to count. See
[`ForceFilter`](../middleware-catalogue/db.md#forcefilter) for the exact rule and
the shapes it deliberately leaves alone.

Refusing the write rather than skipping the recompute is the part that matters.
`BackfillRollups` aggregates by foreign key with no scope at all — it has no
request to take one from — so a row merely left uncounted by the live path would
be folded into the victim's total at the next reconcile.

A write that bypasses the pipeline still bypasses both, so a raw `INSERT` or a
direct adapter call can place a child under any parent. That is the same
exemption raw writes already have from the rollup itself.
