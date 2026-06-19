# 6. Filtering, Sorting & Pagination

The catalogue is in place. This part stitches together the query parameters
exposed by every list endpoint — `filter`, `sort`, `include`, `page`,
`limit` — to build a real browse experience.

## Recap: opt-in fields

Every queryable field carries the relevant `mfx:` tag. `Book`'s fields are
already tagged from Part 3:

| Field | Tags |
|---|---|
| `title` | `filterable,sortable` |
| `isbn` | `filterable,unique` |
| `price` | `filterable,sortable` |
| `stock` | `filterable` |
| `published_at` | `filterable,sortable` |
| `author_id` | `filterable` |

Untagged fields are deliberately invisible to clients — a query string that
references them is rejected with `400 INVALID_QUERY`.

## Filter operators

All the operators on one model:

```bash
# Title contains "wind" (case-insensitive)
curl 'localhost:8080/api/books?filter=title:ilike:%25wind%25'

# Priced between $10 and $20
curl 'localhost:8080/api/books?filter=price:gte:10&filter=price:lte:20'

# Out of stock
curl 'localhost:8080/api/books?filter=stock:eq:0'

# In any of three genres
curl 'localhost:8080/api/books?filter=genres.label:in:Fantasy,Sci-Fi,Mystery'

# Published after a date, returned newest first
curl 'localhost:8080/api/books?filter=published_at:gte:2020-01-01&sort=published_at:desc'
```

Multiple filters compose with AND. The framework parses each filter once in
the Deserialize step into `ctx.Query.Filters`, then the DB step translates
the slice into a `WHERE` clause.

## Relation filters

`?filter=genres.label:in:...` filters through the many-to-many junction.
Dot-notation works on any relation whose target field is `filterable`:

```bash
# Books by an author whose name contains "Le Guin"
curl 'localhost:8080/api/books?filter=author.name:ilike:%25Le+Guin%25&include=author'

# Reviews left on books with a specific ISBN
curl 'localhost:8080/api/reviews?filter=book.isbn:eq:9780061054884&include=book'
```

The include is independent of the filter — you can filter on a relation
without returning it, and vice versa.

## Sorting

`?sort=field:direction` for one column, repeat for tie-breakers:

```bash
# Cheapest first, oldest among ties
curl 'localhost:8080/api/books?sort=price:asc&sort=published_at:asc'
```

Only `sortable` fields work. `BaseModel`'s `created_at` and `updated_at` are
sortable by default.

## Pagination

The defaults — page 1, 20 per page — work everywhere. Override per request:

```bash
curl 'localhost:8080/api/books?page=2&limit=50'
```

`limit` is clamped at 200; oversize requests are silently reduced. List
responses carry pagination metadata in `meta`:

```json
{
  "data":  [ ... ],
  "meta":  { "total": 137, "page": 2, "limit": 50, "pages": 3 }
}
```

For models where the rows are expensive to render — full audit logs,
analytics tables — register `db.Paginate` from the catalogue to lower the
ceiling per model:

```go
import "github.com/xaleel/maniflex/middleware/db"

server.Pipeline.DB.Register(db.Paginate(50), maniflex.ForModel("AuditLog"))
```

## Includes

`?include=relation1,relation2` populates nested objects in the response.
Includes are separate queries — they do not multiply rows or affect
pagination of the primary list:

```bash
curl 'localhost:8080/api/books/<id>?include=author,genres,reviews'
```

The relation keys come from the model declarations — see
[Relations](../defining-your-api/relations.md) for how they are derived. For a `BelongsTo` the
result is a single nested object; for `HasMany` and `ManyToMany`, an array.

## Combining everything

A realistic "browse" call:

```bash
curl 'localhost:8080/api/books?filter=genres.label:eq:Science+Fiction
                              &filter=stock:gt:0
                              &filter=price:lte:20
                              &sort=published_at:desc
                              &include=author,genres
                              &page=1
                              &limit=12'
```

The framework executes this as:

1. Parse query → `ctx.Query.Filters`, `Sorts`, `Includes`, `Page`, `Limit`.
2. Run the main `SELECT` with the WHERE + ORDER BY + LIMIT/OFFSET.
3. Issue follow-up queries for each include, batched by foreign key.
4. Compose the JSON envelope.

## Hardcoding tenant scope

In some applications a request from one customer should never see another
customer's rows. We don't have multi-tenancy in the bookstore, but the
mechanism is worth knowing. `db.Tenancy` enforces row-level scoping
unconditionally:

```go
server.Pipeline.DB.Register(
    db.Tenancy("organization_id", func(ctx *maniflex.ServerContext) string {
        return ctx.Auth.TenantID
    }),
)
```

Once registered, every list, read, update, and delete is silently filtered to
`organization_id = ctx.Auth.TenantID`. The client cannot override or escape
it.

## Custom filters in middleware

`ctx.Query.Filters` is a slice — middleware can append to it before the DB
step runs. We'll use this in Part 7 when we want logged-in customers to see
*only their own orders*:

```go
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
        Field:    "customer_id",
        Operator: maniflex.OpEq,
        Value:    ctx.Auth.UserID,
    })
    return next()
}, maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpList))
```

The pattern is the same as the bigger `Tenancy` middleware — write a filter,
let the DB step honour it.

## Next

In **[Part 7 — Custom Endpoints & Actions](7-actions.md)** we add
order placement: a transactional action that locks stock, creates the order
and its lines, and writes an outbox row that Part 8's background worker will
consume.
