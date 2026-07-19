# Raw Queries & Query Models

The generated CRUD routes cover one-table reads. Anything that needs joins,
aggregates, or custom SQL goes through one of the framework's escape hatches:
raw queries from middleware, or *query models* — read-only models backed by a
hand-written SELECT.

## Raw queries from middleware

`ctx.RawQuery` and `ctx.RawExec` run parameterised SQL through the active
transaction or the bare adapter:

```go
rows, err := ctx.RawQuery(
    `SELECT status, COUNT(*) AS n
       FROM orders
      WHERE organization_id = ?
      GROUP BY status`,
    ctx.Auth.TenantID,
)
```

`rows` is a `[]map[string]any` with column-name keys. Placeholders are rebound to
the adapter's dialect, so `?` works on both SQLite and Postgres (`$1`, `$2`).
**Never** interpolate values into the query string — that's a SQL injection.

`ctx.RawExec` is the same shape for non-`SELECT` statements and returns the
number of rows affected. `ctx.RawQuery` also returns the rows from a
data-modifying statement with a `RETURNING` clause (e.g. `UPDATE … RETURNING id`).

When `ctx.Tx` is non-nil, both methods participate in the active transaction
automatically. The built-in SQLite and Postgres adapters support this; if a
custom adapter's transaction cannot run raw SQL, the call fails with
`maniflex.ErrRawNotSupportedInTx` rather than quietly running on a different
connection outside the transaction — where the write would commit on its own and
survive the rollback.

### Portability pitfalls

Hand-written SQL runs on both SQLite and Postgres, which differ in ways the ORM
normally hides:

- **Parameterise booleans.** `WHERE active = 1` works on SQLite but errors on
  Postgres (`operator does not exist: boolean = integer`). Bind a Go `bool`
  instead: `WHERE active = ?`, `true`.
- **Know your column names.** A column's name comes from the field's `db` tag,
  else its `json` tag, else the snake-cased field name. A camelCase `json` tag
  (`json:"orderId"`) produces a **camelCase column** (`orderId`) — and Postgres
  folds unquoted identifiers in hand-written SQL to lowercase, so `orderId`
  silently won't match. Keep raw SQL to snake_case columns, or set an explicit
  `db:"snake_case"`. SQLite is case-insensitive, so this only bites on Postgres.
- **Pin table names.** Physical table names are pluralised implicitly
  (`VisitorDay` → `visitor_days`). When you reference tables in raw SQL, set
  `ModelConfig.TableName` so the name can't drift from what your SQL expects.

## Structured aggregation: `ctx.Aggregate`

For typed, validated aggregations there's a structured builder that doesn't
require hand-written SQL:

```go
rows, err := ctx.Aggregate("Order", maniflex.AggregateQuery{
    Select: []maniflex.AggregateField{
        {Op: maniflex.AggCount, As: "n"},
        {Op: maniflex.AggSum, Field: "total", As: "revenue"},
    },
    GroupBy: []string{"status"},
    Where: []*maniflex.FilterExpr{
        {Field: "created_at", Operator: maniflex.OpGte, Value: "2026-01-01"},
    },
    Having: []maniflex.HavingClause{
        {Alias: "revenue", Operator: maniflex.OpGt, Value: 1000},
    },
    OrderBy: []maniflex.SortExpr{{DBName: "revenue", Direction: maniflex.SortDesc}},
    Limit:   100,
})
```

Each `AggregateField.Op` is one of `AggCount`, `AggCountDistinct`, `AggSum`,
`AggAvg`, `AggMin`, `AggMax`. Leave `Field` empty on `AggCount` to mean
`COUNT(*)`. `As` overrides the alias used in the result row and in `Having`
or `OrderBy`; if omitted the default is `<op>_<field>` (or `count` for
`COUNT(*)`).

All DB column names — in `Select.Field`, `GroupBy`, and `Where.Field` — are
validated against the registered model. A typo fails fast with a clear
error rather than emitting bad SQL. `OrderBy.DBName` may reference either
an aggregate alias or a `GroupBy` column. Nested-relation filters are not
yet supported in `Aggregate` — use the raw-query escape hatch when you need
them.

When `ctx.Tx` is active the aggregate participates in the transaction,
matching `RawQuery`/`QueryModel`.

To keep an aggregate on a parent column rather than compute it per request —
`Order.PaidAmount` maintained as `SUM(OrderPayment.amount)` — see
[Maintained Rollups](rollups.md), the write-side counterpart of `ctx.Aggregate`.

### Auto-generated aggregate endpoint

Opt a model into a built-in HTTP aggregation route with
`ModelConfig.AggregateEnabled`:

```go
server.MustRegister(Order{}, maniflex.ModelConfig{AggregateEnabled: true})
```

This mounts `GET /:model/aggregate`. The aggregation is described by a JSON
document passed **URL-encoded in the `?aggregate=` query parameter**, and the
group rows come back under the usual `{"data": [...]}` envelope:

```
GET /api/orders/aggregate?aggregate=<url-encoded JSON>

# where the JSON is:
{
  "select":   [{"op": "count", "as": "n"}, {"op": "sum", "field": "amount", "as": "total"}],
  "group_by": ["status"],
  "where":    [{"field": "created_at", "operator": "gte", "value": "2026-01-01"}],
  "having":   [{"alias": "total", "operator": "gt", "value": 1000}],
  "order_by": [{"field": "total", "direction": "desc"}],
  "limit":    100
}
```

```js
const spec = {
  select: [{ op: "sum", field: "amount", as: "total" }],
  group_by: ["status"],
};
const res = await fetch(
  `/api/orders/aggregate?aggregate=${encodeURIComponent(JSON.stringify(spec))}`,
);
```

The spec travels in the query string, not in a request body, because this is a
`GET`: a body on a `GET` is dropped by many proxies and CDNs and cannot be sent
by `fetch()` at all, so an endpoint that needed one worked in development and
failed in production. A request body is not read; sending one gets a
`400 INVALID_AGGREGATE` pointing you at `?aggregate=`.

`op` is one of `count`, `count_distinct`, `sum`, `avg`, `min`, `max` (omit
`field` on `count` for `COUNT(*)`). Field names use the same convention as
`?filter=`/`?sort=` — the JSON name (DB column name also accepted) — and **every
referenced field must be `mfx:"filterable"` or `mfx:"sortable"`**, so the public
endpoint can never aggregate a hidden or sensitive column. WHERE operators are
the flat comparison set plus
`in`/`not_in`/`like`/`ilike`/`contains`/`starts_with`/`ends_with`/`is_null`/`not_null`
(no `between`).

The endpoint runs as the **list** operation: any auth or tenancy middleware you
registered for `OpList` applies unchanged (no separate registration needed), and
request `?filter=` conditions — including middleware-injected tenancy
force-filters — are AND-ed into the aggregate WHERE alongside the spec's own
`where`.

## Tree traversal: `ctx.RecursiveQuery`

For self-referential models — categories, org charts, threaded comments, bill of
materials — `ctx.RecursiveQuery` issues a `WITH RECURSIVE` CTE without hand-writing
SQL:

```go
rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
    RootID:      "some-uuid",
    ParentField: "parent_id",
    MaxDepth:    5,
})
// rows[0]["_depth"] == int64(0) is the root; rows[1..n] are descendants.
```

Every returned row is a `map[string]any` with all the model's columns plus a
synthesised `_depth` integer (0 = the root node). Rows are ordered by `_depth`
ascending.

### Fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `RootID` | `string` | yes | — | Primary key of the starting node |
| `ParentField` | `string` | yes | — | DB column that holds the parent's ID, e.g. `"parent_id"` |
| `Direction` | `RecursiveDirection` | no | `RecursiveDescendants` | Walk downward (`RecursiveDescendants`) or upward (`RecursiveAncestors`) |
| `MaxDepth` | `int` | no | `0` → `DefaultRecursiveMaxDepth` (100) | Stop after this many levels; negative means unlimited |
| `Where` | `[]*FilterExpr` | no | nil | Additional filters applied in both the anchor and recursive members |

### Descendant vs. ancestor traversal

**Descendants** (default) — walks down the tree. Given a root category it
returns all children, grandchildren, etc.:

```go
rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
    RootID:      rootID,
    ParentField: "parent_id",
    // Direction defaults to RecursiveDescendants
})
```

**Ancestors** — walks up the tree. Starting from a leaf, it returns the node
itself, its parent, grandparent, and so on up to the root:

```go
rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
    RootID:      leafID,
    ParentField: "parent_id",
    Direction:   maniflex.RecursiveAncestors,
})
```

### Limiting depth

`MaxDepth: 1` returns the root plus its immediate children only — no further
descendants:

```go
rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
    RootID:      rootID,
    ParentField: "parent_id",
    MaxDepth:    1, // depth 0 (root) + depth 1 (children)
})
```

Left at its zero value, `MaxDepth` applies `maniflex.DefaultRecursiveMaxDepth`
(100) rather than running unbounded. Pass a negative value for a genuinely
unlimited traversal:

```go
MaxDepth: -1, // the whole hierarchy, however deep
```

> Before v0.2.5, `0` meant unlimited. If you relied on that for a hierarchy
> deeper than 100 levels, set `MaxDepth: -1` explicitly.

### Cyclic data

A parent chain that loops — a row that is its own ancestor, whether directly
(`parent_id` pointing at itself) or through a chain — is not an error. The
traversal tracks the ids it has visited and stops at the repeat, so each node is
returned once:

```
A.parent_id = B, B.parent_id = A, root = A
→ A (_depth 0), B (_depth 1)
```

This holds in both directions. It matters because such data is not exotic: a
category tree with no application-level guard against re-parenting a node under
its own descendant will produce one eventually, and before v0.2.5 a single such
request looped until it exhausted the process.

Note that a cycle is the only pathology here. Because the traversal follows one
scalar `ParentField`, every node has exactly one parent and so exactly one path
from the root — the same node cannot be reached twice by different routes.

### Filtering nodes

`Where` filters are applied in both the anchor and recursive members, so a node
that fails the filter is excluded regardless of depth, and the traversal does
not continue through it:

```go
rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
    RootID:      rootID,
    ParentField: "parent_id",
    Where: []*maniflex.FilterExpr{
        {Field: "status", Operator: maniflex.OpEq, Value: "active"},
    },
})
```

Nested-relation filters are not supported in `RecursiveQuery` — use
`ctx.RawQuery` for those cases.

### Soft-delete awareness

When a model uses `WithDeletedAt` or a boolean soft-delete field, the recursive
query automatically excludes deleted records from both the anchor and recursive
members. No extra filter is needed.

### Transaction participation

`RecursiveQuery` participates in `ctx.Tx` exactly like `RawQuery`:

```go
tx, _ := ctx.BeginTx(ctx.Ctx, nil)
ctx.Tx = tx
defer tx.Rollback()

rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{...})
tx.Commit()
```

### Database support

Both Postgres (`$N` placeholders) and SQLite (since 3.8.3, `?` placeholders)
are handled transparently.

## Stable read-only endpoints for aggregates

maniflex has **no** SQL-backed "query model" — a struct cannot be registered with
a SQL body. For a stable, repeatable read endpoint over computed data you have two
real building blocks:

- **The auto-generated aggregate endpoint.** Every model already exposes
  `GET /{model}/aggregate` (see
  [Auto-generated aggregate endpoint](#auto-generated-aggregate-endpoint)),
  driven by [`ctx.Aggregate`](#structured-aggregation-ctxaggregate). Grouping,
  counts, sums/averages, and the standard `?filter=` all apply, and it is in the
  OpenAPI spec — reach for it first for counts/sums/averages over a registered
  model.
- **A custom action running raw SQL.** For a shape the aggregate endpoint cannot
  express (a multi-table join, a window function), mount a
  [custom action](actions.md) whose handler runs `ctx.RawQuery` and returns the
  rows; you own filtering and pagination inside the handler. On Postgres, back an
  expensive aggregate with a materialised view maintained in your migrations and
  `SELECT` from it in the handler.

## When to use which

| Need | Tool |
|---|---|
| One-off aggregate inside an action or middleware | `ctx.RawQuery` |
| Counts / sums / averages as a stable, filterable endpoint | `GET /{model}/aggregate` ([`ctx.Aggregate`](#structured-aggregation-ctxaggregate)) |
| A bespoke read endpoint (joins, window functions) | custom [action](actions.md) + `ctx.RawQuery` |
| Tree traversal (descendants, ancestors, depth limit) | `ctx.RecursiveQuery` |
| Bulk mutation inside a single request | `ctx.RawExec` (inside a transaction) |
| Per-row business logic across many rows | [Batch Operations & Sagas](batch-saga.md) |

## Performance notes

- Raw queries do not cache; each request executes the SQL. For a frequently-hit
  aggregate exposed through a custom action, wrap it with `response.Cache` (see
  [Response Middleware](../middleware-catalogue/response.md)) or maintain a summary
  table (the write-side [Maintained Rollups](rollups.md) are built for this).
- Avoid unbounded scans — add `WHERE` and `LIMIT` clauses to any hand-written SQL
  when the underlying table is large.
- For Postgres, a materialised view often beats recomputing an expensive
  aggregate per request; refresh it on a schedule and read it from the handler.
