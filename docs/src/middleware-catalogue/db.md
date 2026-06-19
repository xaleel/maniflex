# DB Middleware

The `maniflex/middleware/db` package wraps the **DB** step with row-level
scoping, request budgeting, post-write hooks, and result caching.

## Row-level scoping

### `ForceFilter`

Injects a filter on every list, read, update, and delete, regardless of what
the client requested. Used to enforce invariants the client cannot override:

```go
import "github.com/xaleel/maniflex/middleware/db"

server.Pipeline.DB.Register(
    db.ForceFilter("org_id", func(ctx *maniflex.ServerContext) any {
        return ctx.Auth.Claims["org_id"]
    }),
)
```

### `Tenancy`

A specialised `ForceFilter` for the common multi-tenant case. Reads the tenant
id from `ctx.Auth` and pins every query to it:

```go
server.Pipeline.DB.Register(
    db.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
        return ctx.Auth.Claims["org_id"].(string)
    }),
)
```

`Tenancy` also rewrites the `org_id` field on creates and updates so a tenant
cannot place rows into another tenant's bucket.

## Request budgeting

### `Paginate`

Caps the maximum `?limit=` accepted on list responses. Per-model overrides are
common for tables whose rows are expensive to render:

```go
server.Pipeline.DB.Register(db.Paginate(50), maniflex.ForModel("AuditLog"))
```

### `RateLimit`

A token-bucket rate limiter scoped by IP or by authenticated user:

```go
server.Pipeline.DB.Register(
    db.RateLimit(db.RateLimitConfig{
        RequestsPerMinute: 10,
        Key: func(ctx *maniflex.ServerContext) string {
            if ctx.Auth != nil {
                return ctx.Auth.UserID
            }
            return ctx.Request.RemoteAddr
        },
    }),
    maniflex.ForModel("PasswordReset"),
)
```

Rejected requests receive `429 RATE_LIMITED`.

## Post-write hooks

These run at `maniflex.After` position so they only fire when the database write
succeeded.

### `AuditLog`

Writes one audit record per mutating operation to a configured sink:

```go
server.Pipeline.DB.Register(
    db.AuditLog(mySink),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)
```

`mySink` is anything implementing the audit interface — a logger, a database
table, an external SIEM. The record carries the operation, model name, actor
(from `ctx.Auth`), and a JSON diff of the affected row.

### `Invalidate`

Invalidates cache keys when a row changes. The key list is computed per
request:

```go
server.Pipeline.DB.Register(
    db.Invalidate(redisCache, func(ctx *maniflex.ServerContext) []string {
        return []string{
            "posts:list",
            fmt.Sprintf("post:%s", ctx.ResourceID),
        }
    }),
    maniflex.ForModel("Post"),
    maniflex.AtPosition(maniflex.After),
)
```

### `CacheQuery`

Memoises read results (`OpRead`, `OpList`) in a `CacheStore` — the read-side
complement to `Invalidate`. On a cache hit it sets `ctx.DBResult` and the
adapter read is skipped; the Response step renders the cached result. On a miss
it runs the read and stores the result for `TTL`. Pair it with `Invalidate` on
writes to evict stale entries.

```go
cache := maniflex.NewMemoryCache() // or a Redis-backed CacheStore

server.Pipeline.DB.Register(
    db.CacheQuery(cache, db.CacheConfig{
        TTL:     5 * time.Minute,
        KeyFunc: func(ctx *maniflex.ServerContext) string {
            // Only cache the common, high-traffic query shapes. Requests that
            // filter on `name` or carry a ?q= search are typically long-tail,
            // ad-hoc lookups — caching them floods the store with low-value
            // entries that are rarely read back, so skip them by returning "".
            if q := ctx.Query; q != nil {
                if q.Search != "" {
                    return ""
                }
                for _, f := range q.Filters {
                    if f.Field == "name" {
                        return ""
                    }
                }
            }
            return "products:list:" + ctx.Request.URL.RawQuery
        },
    }),
    maniflex.ForModel("Product"),
    maniflex.ForOperation(maniflex.OpList, maniflex.OpRead),
)
server.Pipeline.DB.Register(
    db.Invalidate(cache, func(*maniflex.ServerContext) []string {
        return []string{"products:list:..."}
    }),
    maniflex.ForModel("Product"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)
```

`KeyFunc` must capture every input that changes the result (model, tenant,
filters, sort, pagination, includes); returning `""` skips the cache for that
request. The value stored is `ctx.DBResult`, so a distributed `CacheStore` must
round-trip a `*maniflex.ListResult` for lists — a store that decodes list
entries into a bare map is treated as a miss rather than panicking. Avoid
caching `mfx:"encrypted"` models, since the decrypted result would live in the
cache.

## Ordering

Row-level scopers (`ForceFilter`, `Tenancy`) must run **Before** the default DB
step — the default position — so the filter is in place when the SELECT or
UPDATE runs. Post-write hooks must run **After**, so they observe the result.
The package's defaults follow this; only change them if you know why.
