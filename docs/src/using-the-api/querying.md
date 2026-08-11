# Querying

Every generated list and read endpoint accepts the same query parameters —
`page`, `limit`, `filter`, `sort`, `include`, and `select`. This page
documents their grammar and the fields that opt in to each.

## Query complexity limits

Client-controlled query shapes are bounded before SQL is built. Defaults allow
at most 8 KiB for the complete request URI, 32 filter clauses, 8 bracketed OR
groups with 8 clauses each, 8 sort fields, 64 selected fields, and 8 include
paths. Exceeding the URI ceiling returns `414 URI_TOO_LONG`; exceeding a shape
limit returns `400 INVALID_QUERY`.

Applications can change these through `Config.QueryLimits`, and can override
individual fields for one model with `ModelConfig.QueryLimits`. A zero field
inherits; a negative field explicitly disables that limit. The router-level
global URI ceiling cannot be loosened per model. See
[Configuration](../deployment/config.md#limits) for every field and default.

## `page` and `limit`

Standard offset pagination.

```
?page=2&limit=20
```

| Parameter | Default | Maximum |
|---|---|---|
| `page` | `1` | `1,000,000` |
| `limit` | `20` | `200` |

Limits above the maximum are clamped silently. Pages above the maximum, values
whose pagination arithmetic cannot fit, and negative or non-numeric values are
rejected with `400 INVALID_QUERY`.

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

The cursor column must be `mfx:"sortable"`, **not nullable**, and a supported
scalar type (`string`, `bool`, an integer, a float, or `time.Time`). Pointer,
collection, and structured fields are rejected at registration. Keyset pagination
needs a total order, and NULL has no place in one: the boundary comparison is never
true for it, and Postgres and SQLite don't even agree where NULLs sort. A nullable
cursor column would drop or repeat rows across pages, so the model fails to
register rather than paginating wrongly.

When the cursor column is one of `BaseModel`'s — `created_at` is the usual
choice — `sortable` is not a default and `cursor_field` does not grant it
implicitly. Declare both:

```go
type Event struct {
    maniflex.BaseModel `mfx:"cursor_field:created_at"`
    Name               string `json:"name" mfx:"required"`
}

server.MustRegister(Event{}, maniflex.ModelConfig{
    BaseModelTags: map[string]string{"created_at": "sortable,index"},
})
```

Writing one half without the other fails registration with an error naming the
missing piece.

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
opaque — treat it as a string and pass it back verbatim. Tokens carry the cursor
value's type and are checked against the model field; a token for a different
field type is rejected with `400 INVALID_QUERY`. Missing IDs, null or non-scalar
values, trailing JSON, and values outside the field or database driver's supported
range are rejected the same way before query execution. Timestamp cursors use a
fixed-width UTC representation so ordering is identical on SQLite and Postgres.
Valid scalar unversioned tokens issued by earlier Maniflex releases remain
accepted during upgrades.

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

### Combining filters with OR

Add a bracketed index to put filters in the same **OR group**. Filters sharing
an index are OR-ed together:

```
?filter[0]=status:eq:draft&filter[0]=status:eq:published
```

Different indexes are separate groups, and groups combine with AND. A bare
`?filter=` has no group and is its own AND clause, so the two spellings mix
freely:

```
# (owner = u1 OR owner = u2) AND amount >= 20
?filter[0]=owner:eq:u1&filter[0]=owner:eq:u2&filter[1]=amount:gte:20

# resolved = false AND (owner = u1 OR owner = u2)
?filter=resolved:eq:false&filter[0]=owner:eq:u1&filter[0]=owner:eq:u2
```

That is the whole expressible shape: **an AND of ORs**. There is no way to OR
across groups, and no nesting — `(a AND b) OR (c AND d)` cannot be written as a
query string. When you need it, put the query behind a
[custom action](../advanced-topics/actions.md) and write it with
[`ctx.RawQuery`](../advanced-topics/raw-queries.md), where the shape is yours to
choose and is not client-controlled.

The index is a label, not an ordering: `filter[7]` and `filter[2]` are simply
two groups, and gaps are fine. It must be a non-negative integer — `filter[recent]`
is refused with `400 INVALID_QUERY` naming the requirement rather than being
ignored.

Group counts are bounded by `Config.QueryLimits` — by default 8 groups of 8
clauses, inside an overall ceiling of 32 filter clauses. See
[Query complexity limits](#query-complexity-limits) above.

The `/aggregate` endpoint reads `?filter=` with exactly these semantics, so a
grouped filter counts the rows the same filter lists.

> **Go callers:** the equivalent is `FilterExpr.Group`. Any value ≥ 1 is a
> group; `0` is the zero value and means ungrouped, so groups start at 1 rather
> than 0 and the numbering does **not** line up with the URL's — `filter[0]`
> parses to `Group: 1`.

```go
filters := []*maniflex.FilterExpr{
    // (owner = u1 OR owner = u2) AND amount >= 20
    {Field: "owner", Operator: maniflex.OpEq, Value: "u1", Group: 1},
    {Field: "owner", Operator: maniflex.OpEq, Value: "u2", Group: 1},
    {Field: "amount", Operator: maniflex.OpGte, Value: 20}, // ungrouped → AND
}
```

### Values are read against the column's type

A filter value arrives as text — a URL has no types — and is coerced to the form
its column compares against, so the same filter means the same thing on every
driver.

A **boolean** column accepts `true`/`false` and `1`/`0`, in any case:

```
?filter=resolved:eq:false
?filter=resolved:eq:0        # identical
?filter=resolved:in:true,false
```

This matters more than it looks. A caller interpolating a boolean into a URL
(`` `resolved:eq:${showResolved}` ``) sends the word, and a bound string is not
the same thing as a SQL literal: on SQLite, where booleans are stored in an
`INTEGER` column, the word `false` stays TEXT and can never compare equal, so
the filter used to return **zero rows with no error** while the identical
request against Postgres worked. Both drivers now agree.

A **timestamp** column takes any full RFC3339 value, with or without a fraction
and in any zone; it is normalised to UTC in a fixed-width form so that string
comparison on SQLite orders the same way instants do. A date-only bound
(`2026-01-01`) is left exactly as written and keeps its meaning.

A value that is not a recognised spelling for its column is passed through
untouched rather than guessed at.

`BaseModel`'s `id`, `created_at` and `updated_at` are **not** filterable by
default — the columns are `readonly` and nothing more. A model opts them in at
registration, since `BaseModel` lives in the framework and its struct tags
cannot be edited:

```go
server.MustRegister(Post{}, maniflex.ModelConfig{
    BaseModelTags: map[string]string{"created_at": "filterable,sortable"},
})
```

See [BaseModel](../defining-your-api/models.md#querying-the-basemodel-columns)
for the per-column allowlist.

### Operators

| Operator | Effect | Value |
|---|---|---|
| `eq` | field = value | one value |
| `neq` | field ≠ value | one value |
| `gt`, `gte`, `lt`, `lte` | numeric and date comparisons | one value |
| `like` | SQL `LIKE`, case-sensitive | one **pattern** — `%` and `_` are wildcards |
| `ilike` | SQL `ILIKE`, case-insensitive | one **pattern** — `%` and `_` are wildcards |
| `contains` | field contains the value, case-insensitive | one literal value |
| `starts_with` | field starts with the value, case-insensitive | one literal value |
| `ends_with` | field ends with the value, case-insensitive | one literal value |
| `in` | field IN (…) | at least one comma-separated value |
| `not_in` | field NOT IN (…) | at least one comma-separated value |
| `between` | field ≥ lo AND ≤ hi (inclusive) | exactly two comma-separated values `lo,hi` |
| `is_null` | field IS NULL | no value |
| `not_null` | field IS NOT NULL | no value |
| `eq_field`, `neq_field` | field = / ≠ **another column** | the name of another column |
| `gt_field`, `gte_field`, `lt_field`, `lte_field` | comparisons against **another column** | the name of another column |

```
?filter=tag:in:go,rust,zig
?filter=amount:between:100,500
?filter=created_at:between:2025-01-01,2025-03-31
?filter=archived_at:is_null
?filter=title:ilike:%intro%
?filter=title:contains:intro
?filter=paid_amount:gte_field:amount_due
```

### Patterns vs. literals

`like` and `ilike` take a **pattern**: `%` matches any run of characters and `_`
matches exactly one. That is what makes `?filter=title:ilike:%intro%` work — but
it also means a value the user typed is interpreted rather than matched.
`?filter=label:like:50%` finds `500 units` and `50 off` as readily as the `50%`
you were looking for, and there is no portable way to escape a `%` in a pattern
(SQLite has no escape character by default; Postgres has a backslash).

`contains`, `starts_with`, and `ends_with` take a **literal**: `%` and `_` in the
value are escaped for you and match themselves, so `?filter=label:contains:50%`
finds exactly the labels containing `50%`. They are case-insensitive on both
backends. Use them for anything a user typed — a search box, a filename, an SKU —
and reach for `like`/`ilike` only when the caller genuinely is writing a pattern.

Note that `%` must still be percent-encoded in a URL (`%25`), as in any query
string:

```
?filter=label:contains:50%25       → matches the literal "50%"
?filter=label:like:50%25           → matches "50%", "500 units", "50 off", …
```

### Comparing two columns

The `*_field` operators compare one column against another column of the same
record, instead of against a value you supply:

```
?filter=paid_amount:gte_field:amount_due     # settled orders
?filter=paid_amount:lt_field:amount_due      # orders still owing
```

The value is a **field name**, never a literal — that is why these are separate
operators rather than a marker on the value. `?filter=note:eq:status` compares
the `note` column against the *text* `"status"`; `?filter=note:eq_field:status`
compares it against the `status` **column**. Neither spelling can be mistaken for
the other.

Both sides must:

- be columns on the model you are listing — a relation (`customer.credit`) or a
  locale sub-key is rejected with `400 INVALID_QUERY`;
- be marked `mfx:"filterable"`, the same tag the left side of any filter needs;
- hold the same kind of value. Numbers compare with numbers, strings with
  strings, booleans with booleans, timestamps with timestamps. Mixing them —
  `?filter=paid_amount:gte_field:note` — is rejected rather than left to the
  database, because SQLite and PostgreSQL would not agree on what it means.

Encrypted columns cannot be compared, for the same reason they cannot be
filtered: their stored ordering is not their plaintext ordering.

If either column is `NULL` on a row, the comparison is `NULL` rather than true,
so the row is excluded — from `neq_field` as well as from `eq_field`. Add an
explicit `?filter=credit:not_null` when you need those rows counted.

Arithmetic is not supported: there is no way to write
`paid_amount >= amount_due + delivery_fee`. Maintain the total you want to
compare against as its own column.

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

Only fields tagged `mfx:"sortable"` may be used. `BaseModel`'s `id`,
`created_at` and `updated_at` are **not** sortable by default — opt in with
`ModelConfig.BaseModelTags` as shown under [`filter`](#filter) above. A sort on
a column that has not opted in returns `400`, and the error names
`BaseModelTags` as the fix.

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

### One level of nesting

A key may carry a single dot to load a relation of the related model:

```
?include=author.company
?include=author.company,comments
```

`?include=author.company` implies `author` — the parent is what the child hangs
off — so you do not need to name both.

**Two segments is the limit.** `?include=a.b.c` is refused with `400
INVALID_QUERY`. Each level is one more batched query, and the tree comes from the
client, so leaving it uncapped would let a caller choose how much work a request
costs.

Every segment must name a real relation; a typo is a `400`, not a silently
missing key. Nested rows are scoped, decrypted and field-filtered exactly as the
first level is — a forced filter (`db.Tenancy`, `db.ForceFilter`) applies at every
level, and `hidden` / `writeonly` fields on the nested model stay out.

> **Go callers:** nesting applies to the JSON response. The typed relation
> structs (`post.Author`) are still populated one level deep, so
> `post.Author.Company` is not filled in by a typed read. Use the JSON path, or a
> second `maniflex.Read`.

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
