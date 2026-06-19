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

`rows` is a `[]map[string]any` with column-name keys. Use driver-appropriate
placeholders (`$1`, `$2` for Postgres; `?` for SQLite). **Never** interpolate
values into the query string — that's a SQL injection.

`ctx.RawExec` is the same shape for non-`SELECT` statements and returns the
number of rows affected.

When `ctx.Tx` is non-nil, both methods participate in the active transaction
automatically.

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

### Auto-generated aggregate endpoint

Opt a model into a built-in HTTP aggregation route with
`ModelConfig.AggregateEnabled`:

```go
server.MustRegister(Order{}, maniflex.ModelConfig{AggregateEnabled: true})
```

This mounts `GET /:model/aggregate`, which accepts a JSON body describing the
aggregation and returns the group rows under the usual `{"data": [...]}`
envelope:

```
GET /api/orders/aggregate
{
  "select":   [{"op": "count", "as": "n"}, {"op": "sum", "field": "amount", "as": "total"}],
  "group_by": ["status"],
  "where":    [{"field": "created_at", "operator": "gte", "value": "2026-01-01"}],
  "having":   [{"alias": "total", "operator": "gt", "value": 1000}],
  "order_by": [{"field": "total", "direction": "desc"}],
  "limit":    100
}
```

`op` is one of `count`, `count_distinct`, `sum`, `avg`, `min`, `max` (omit
`field` on `count` for `COUNT(*)`). Field names use the same convention as
`?filter=`/`?sort=` — the JSON name (DB column name also accepted) — and **every
referenced field must be `mfx:"filterable"` or `mfx:"sortable"`**, so the public
endpoint can never aggregate a hidden or sensitive column. WHERE operators are
the flat comparison set plus `in`/`not_in`/`like`/`ilike`/`is_null`/`not_null`
(no `between`).

The endpoint runs as the **list** operation: any auth or tenancy middleware you
registered for `OpList` applies unchanged (no separate registration needed), and
request `?filter=` conditions — including middleware-injected tenancy
force-filters — are AND-ed into the aggregate WHERE alongside the body's own
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
| `MaxDepth` | `int` | no | `0` (unlimited) | Stop after this many levels; `0` means traverse the whole subtree |
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

## Read-only query models

A *query model* is a struct registered with a SQL body instead of a table.
The framework mounts the standard list/read routes, including filtering,
sorting, and pagination, but every read runs the supplied SQL.

```go
type RevenueByMonth struct {
    maniflex.BaseModel
    Month   string  `json:"month"   mfx:"filterable,sortable"`
    Total   float64 `json:"total"   mfx:"sortable"`
    Orders  int64   `json:"orders"  mfx:"sortable"`
}

server.MustRegister(RevenueByMonth{}, maniflex.ModelConfig{
    QueryModel: &maniflex.QueryModelSpec{
        SQL: `SELECT to_char(created_at, 'YYYY-MM') AS month,
                     SUM(total) AS total,
                     COUNT(*) AS orders
                FROM orders
               WHERE status = 'paid'
               GROUP BY month`,
    },
})
```

Behaviour:

- `GET /revenue_by_months` runs the SQL, applies any client-supplied
  `?filter`, `?sort`, and `?page` / `?limit` against the resulting columns, and
  paginates the result.
- `POST` / `PATCH` / `DELETE` are not mounted — query models are read-only.
- The struct's `mfx:` tags still apply: `filterable` opens a column to
  `?filter=`, `sortable` to `?sort=`, `hidden` and `writeonly` are honoured.
- The model participates in OpenAPI generation, so the endpoint is documented
  in `/openapi.json` like any other.

## When to use which

| Need | Tool |
|---|---|
| One-off aggregate inside an action or middleware | `ctx.RawQuery` |
| Aggregation that should be a stable, paginated, filterable endpoint | Query model |
| Tree traversal (descendants, ancestors, depth limit) | `ctx.RecursiveQuery` |
| Bulk mutation inside a single request | `ctx.RawExec` (inside a transaction) |
| Per-row business logic across many rows | [Batch Operations & Sagas](batch-saga.md) |

Query models are the better choice when an external consumer needs to call the
endpoint repeatedly — the API surface is stable, filterable, documented, and
auto-generated alongside the rest. Raw queries are the better choice for one-shot
work inside an action.

## Performance notes

- Query models do not cache; each request executes the SQL. For frequently-hit
  aggregates, wrap with `response.Cache` (see
  [Response Middleware](../middleware-catalogue/response.md)) or maintain a summary
  table.
- The framework treats the SQL as a subquery; client filters become `WHERE`
  clauses against the result columns. Avoid unbounded scans — add `WHERE` and
  `LIMIT` clauses to the SQL itself when the underlying table is large.
- For Postgres, a materialised view often beats a query model for expensive
  aggregates. The query model can then `SELECT` from the materialised view.
