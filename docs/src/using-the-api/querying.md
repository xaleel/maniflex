# Querying

Every generated list and read endpoint accepts the same query parameters —
`page`, `limit`, `filter`, `sort`, `include`, and `select`. This page
documents their grammar and the fields that opt in to each.

## `page` and `limit`

Standard offset pagination.

```
?page=2&limit=20
```

| Parameter | Default | Maximum |
|---|---|---|
| `page` | `1` | unbounded |
| `limit` | `20` | `200` |

Values above the maximum are clamped silently. Negative or non-numeric values
are rejected with `400 INVALID_QUERY`.

The response carries `meta.total`, `meta.page`, `meta.limit`, and `meta.pages`
— see [Response Envelope](responses.md).

## `cursor` (keyset pagination)

Offset pagination skips or duplicates rows when the dataset changes between
page fetches — delete a row on page 1 and page 2 silently jumps a record.
Keyset (cursor) pagination walks the data by a stable ordering key instead, so
the window never shifts. Opt a model in by naming a sortable, effectively
monotonic cursor column:

```go
type Event struct {
    maniflex.BaseModel `mfx:"cursor_field:created_at"` // created_at is sortable on BaseModel
    Name string        `json:"name" db:"name"`
}
```

Equivalently, set `ModelConfig.CursorField: "created_at"` at registration, or
put `mfx:"...,cursor_field:<name>"` on any of the model's own fields.

The presence of `?cursor=` switches the request into keyset mode (it supersedes
`?page`). Send an empty value for the first page, then the `meta.next_cursor`
from each response to fetch the next:

```
GET /events?cursor=&limit=20          → first page
GET /events?cursor=<next_cursor>&limit=20  → following page
```

The walk is ordered by `(cursor_field, id)` — `id` is the implicit tiebreaker so
the order is total even when the cursor column ties. The default direction is
ascending; sort on the cursor field to reverse it:

```
GET /events?cursor=&sort=created_at:desc
```

Any `?sort=` on a *different* field is rejected with `400` in cursor mode, since
the keyset order is fixed to the cursor column.

Cursor responses carry a different `meta` shape — no `total`/`page`/`pages`
(the count is skipped, which is the point on large tables):

```json
{ "data": [ ... ], "meta": { "limit": 20, "next_cursor": "eyJ2Ijoi...", "has_more": true } }
```

`has_more` is `false` and `next_cursor` is omitted on the last page. The token is
opaque — treat it as a string and pass it back verbatim.

## `filter`

Each filter is a colon-separated triple — *field*, *operator*, *value*:

```
?filter=status:eq:published
?filter=views:gt:100
?filter=created_at:gte:2025-01-01
```

Multiple filters combine with AND:

```
?filter=status:eq:published&filter=views:gt:100
```

Filters reference a field by its `json` name. Only fields tagged
`mfx:"filterable"` may be used; unknown or non-filterable references abort the
request with `400 INVALID_QUERY`.

### Operators

| Operator | Effect | Value |
|---|---|---|
| `eq` | field = value | one value |
| `neq` | field ≠ value | one value |
| `gt`, `gte`, `lt`, `lte` | numeric and date comparisons | one value |
| `like` | SQL `LIKE`, case-sensitive | one value, `%` wildcards |
| `ilike` | SQL `ILIKE`, case-insensitive | one value, `%` wildcards |
| `in` | field IN (…) | comma-separated values |
| `not_in` | field NOT IN (…) | comma-separated values |
| `between` | field ≥ lo AND ≤ hi (inclusive) | exactly two comma-separated values `lo,hi` |
| `is_null` | field IS NULL | no value |
| `not_null` | field IS NOT NULL | no value |

```
?filter=tag:in:go,rust,zig
?filter=amount:between:100,500
?filter=created_at:between:2025-01-01,2025-03-31
?filter=archived_at:is_null
?filter=title:ilike:%intro%
```

### Filtering on related fields

When a relation is declared on the model, you can filter by a field on the
*related* table using dot notation:

```
?filter=user.role:eq:admin
?filter=posts.status:eq:published
```

The related field must itself be `filterable`. The framework joins the related
table for the query; no separate `?include=` is required to filter on it (but
you still need `?include=` to *return* the related row).

## `q` (full-text search)

`?q=` runs a native full-text search over every field tagged `mfx:"searchable"`
and orders the results by match relevance:

```
?q=hello world
?q=postgres&filter=tag:eq:db
```

This is distinct from `filter`: full-text search uses the database's own
ranking, stemming, and tokenisation rather than literal comparison, so `?q=run`
also matches *running*, and the densest match ranks first. The backend's native
machinery does the work — a `tsvector` column and GIN index on PostgreSQL, an
FTS5 index on SQLite — both provisioned automatically during migration.

- Only models with at least one `mfx:"searchable"` field accept `?q=`; on any
  other model it aborts with `400 INVALID_QUERY`. Searchable fields must be text
  columns.
- `?q=` combines with `?filter=` (ANDed) and the usual `?page=`/`?limit=`
  offset pagination. It cannot be combined with `?cursor=`, since keyset order
  and relevance order are mutually exclusive.
- An empty value (`?q=`) is ignored — the list is returned unfiltered.
- On PostgreSQL the text-search configuration defaults to `english`; override it
  per model with `ModelConfig.SearchLanguage`.

```go
type Article struct {
    maniflex.BaseModel
    Title string `json:"title" db:"title" mfx:"required,searchable"`
    Body  string `json:"body"  db:"body"  mfx:"searchable"`
}
// GET /articles?q=keyset+pagination → relevance-ranked matches
```

## `sort`

Each sort is `field:direction`:

```
?sort=created_at:desc
?sort=title:asc
```

Multiple sorts compose left-to-right (primary, secondary, …):

```
?sort=status:asc&sort=created_at:desc
```

Only fields tagged `mfx:"sortable"` may be used. `BaseModel`'s `created_at` and
`updated_at` are sortable by default.

### Sorting on a relation field

Use `relation.field` to sort by a column on a `BelongsTo` parent. The server
adds the LEFT JOIN automatically — no `filter` or `include` on that relation is
required:

```
?sort=user.name:asc
?sort=vendor.name:desc&filter=status:eq:open
```

The related field must be tagged `mfx:"sortable"` on the parent model. Only
`BelongsTo` relations are supported; `relation.field` on a `HasMany` or
`ManyToMany` returns `400`, as does an unknown relation or a non-sortable
related field.

## `include`

Loads related records inline. The value is a comma-separated list of relation
keys:

```
?include=user
?include=user,comments
```

Each key becomes a nested object (for `BelongsTo`) or array (for `HasMany` and
`ManyToMany`) on the returned row. See [Relations](../defining-your-api/relations.md) for how
relation keys are derived.

Includes are populated by separate queries after the main query — they do not
multiply rows or affect pagination.

## `select`

Request a subset of fields instead of the full row. Useful for wide tables
(payroll, product catalogues with 40+ attributes) where most columns are
irrelevant to the caller.

```
?select=id,name,department
?select=id,amount,status
```

The value is a comma-separated list of **JSON field names**. Unknown names
abort the request with `400 INVALID_QUERY`. Fields tagged `mfx:"hidden"` or
`mfx:"writeonly"` are still stripped from the response even if explicitly
selected — the projection happens at the database layer, not as an ACL bypass.

`?select=` applies to both **list** (`GET /:model`) and **read**
(`GET /:model/:id`) endpoints. It can be combined freely with `filter`, `sort`,
and `include`.

## Putting it together

A complete request that exercises all parameters:

```
GET /api/posts
    ?filter=status:eq:published
    &filter=views:gte:100
    &sort=created_at:desc
    &include=user,comments
    &select=id,title,views,status
    &page=1
    &limit=20
```

The framework parses the query string once in the Deserialize step into
`ctx.Query` (a `*QueryParams`), which middleware can read and modify before
the DB step. Tenant-scoping middleware, for example, appends a filter to
`ctx.Query.Filters` to enforce row-level access — see
[Example 2](../the-request-pipeline/example-2.md).
