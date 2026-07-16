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

On a list or a read the filter goes into the query. On an update or a delete
there is nowhere to put it — the adapter's `Update` and `Delete` are keyed by id
alone — so the DB step reads the record back through the filter first and
answers `404` if it does not match, indistinguishably from a record that is
genuinely absent. The check and the write share one transaction, so the row
cannot leave scope in between.

That read is the only cost, and only a request carrying a forced filter pays it:
a write with nothing scoped goes straight to the adapter as before. A client's
own `?filter=` never constrains a write — only filters the server imposed do.

If you build a `maniflex.FilterExpr` by hand and it expresses **who may touch the
row** rather than **which rows were asked for**, set `Forced: true` on it; that is
what carries it onto updates and deletes. `ForceFilter` and `Tenancy` set it for
you.

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
cannot place rows into another tenant's bucket. That rewrite is why the scoping
on updates matters twice over: it stamps the caller's tenant onto whatever row
the update reaches, so an update that reached the wrong row would not merely
overwrite it — it would move it into the caller's tenant and leave the owner
unable to see it at all.

### Scoping Actions: `TenancyAction` / `ForceFilterAction`

`Tenancy` and `ForceFilter` register on the **DB step**, and a custom `Action`
does not run it — its pipeline is `Auth → middleware → handler → Response`. Their
only output is a filter on `ctx.Query`, which nothing in that chain reads, so
registering them on the DB step does nothing at all for an Action, silently.

Use the `Action` variants instead, in the action's own `Middleware` list, where
they run after Auth and can read `ctx.Auth`:

```go
server.Action(maniflex.ActionConfig{
    Method: "POST", Path: "/orders/{id}/refund",
    Middleware: []maniflex.MiddlewareFunc{
        auth.JWTAuth(secret),
        db.TenancyAction("org_id", func(ctx *maniflex.ServerContext) string {
            return ctx.Auth.Claims["org_id"].(string)
        }),
    },
    Handler: refund,
})
```

Inside that handler every DB path either applies the scope or refuses to run:

| Path | Under a scope |
|---|---|
| `ctx.GetModel(name)` — List/Read/Create/Update/Delete | scoped |
| `maniflex.List/Read/Create/Update/Delete[T]` | scoped |
| `ctx.Aggregate` | scoped (AND-ed into `WHERE`) |
| `ctx.LockForUpdate` | scoped |
| `ctx.BeginTx` — and the `Tx` it returns | scoped |
| `ctx.RawQuery`, `ctx.RawExec` | **refuses** |
| `ctx.Search`, `ctx.RecursiveQuery` | **refuses** |

Reads see only matching rows. A create is stamped with the scope's values,
overwriting whatever the caller supplied — a row created outside the scope would
be invisible to the caller that created it. An update or delete of a record
outside the scope returns `ErrNotFound`, the same answer the scoped read gives.

Transactions work normally. `Tx` mirrors `DBAdapter` — its `FindByID`/`FindMany`
take a `*QueryParams`, its `Update`/`Delete` are keyed by id — so the `Tx` that
`ctx.BeginTx` returns is scoped the same way the accessor is, and that matters
because `ctx.Tx` is a public field: anything downstream that picks the
transaction up is scoped too. `maniflex.WithTransaction` and `maniflex.Batch`
both call `ctx.BeginTx`, so both work on a scoped action:

```go
tx, err := ctx.BeginTx(ctx.Ctx, nil)  // scoped
defer tx.Rollback()
// tx.FindByID / tx.Update / tx.Delete all honour the scope
return tx.Commit()
```

**The refusals are the point.** Raw SQL is opaque to the framework: a `SELECT`
string cannot be scoped without rewriting it. Scoping the convenient paths and
letting those leak in silence would put the guarantee in this page and not in the
code — worse than no guarantee, because it would be trusted. Refusing means an
action either honours the scope or fails at the first request that exercises it.

When a path genuinely must step outside the scope — an audit query across
tenants, a migration — say so:

```go
rows, err := ctx.Unscoped().RawQuery("SELECT ... FROM orders")
```

`ctx.Unscoped()` exposes `RawQuery`, `RawExec`, `BeginTx`, `GetModel` and
`Search` with no scope applied. It is a distinct call rather than a flag so the
bypass shows up at the call site, in the diff, and in a grep — which only works
while it stays rare, so it is deliberately not needed for ordinary work.

The scope applies only to Actions. A generated CRUD route gets its scoping from
the DB step, where `ctx.RawQuery` from an After-DB middleware is a normal thing
to do and is not refused.

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

When the key falls back to the client IP (`ctx.Request.RemoteAddr`), that address
is the direct TCP peer unless `Config.TrustProxyHeaders` is enabled — set it
(only behind a trusted proxy) so per-IP limits see the real client instead of the
load balancer. See [Security](../advanced-topics/security.md).

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
