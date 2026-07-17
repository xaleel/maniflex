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

`Forced: true` also lets a scope be imposed **before** the DB step. The
Deserialize step rebuilds `ctx.Query` from the request, which discards a plain
filter an earlier step appended — but a `Forced` filter survives that rebuild.
So an Auth-step middleware may append a `Forced` filter to `ctx.Query.Filters`
and have it reach the query; a non-forced one set that early is dropped. The DB
step remains the idiomatic home for scoping (`ForceFilter`, `Tenancy`), and is
required when the scope must also cover writes end-to-end within one transaction.

Use the `maniflex.Op*` constants for `Operator`. It's a bare string type, and a
hand-built filter is never parsed — only filters arriving over HTTP are — so
`Operator: "equals"` compiles and boots, and until v0.2.3 it produced a scope that
silently matched every row. An operator no adapter implements is now refused with
an error naming it, and the adapters render it as a false predicate rather than a
true one, so a filter reaching one by some other path matches nothing instead of
everything. `maniflex.FilterOperator.Valid()` reports whether an operator is one
the query builder implements.

### `ForceFilterVia`

`ForceFilter` maps a field to a value, which needs the model to carry that field.
A child table often doesn't: a `DamagedItem` has an `item_id` and nothing else,
and whether it's yours is a fact about its `Item`. `ForceFilterVia` scopes such a
model through the column its parent carries:

```go
// A DamagedItem is the caller's if its Item is
server.Pipeline.DB.Register(
    db.ForceFilterVia("item", "owner_id", func(ctx *maniflex.ServerContext) any {
        return ctx.Auth.Claims["owner_id"]
    }),
    maniflex.ForModel("DamagedItem"),
)
```

The first argument names the relation, in the same vocabulary a nested `?filter=`
uses (`?filter=author.status:neq:banned` → `author`); the second names a column on
the parent, by JSON or DB name. The relation must be a `BelongsTo` — the join needs
a foreign key on this row pointing at the one row that owns it, which is what a
`HasMany` doesn't have.

The alternative is to denormalise `owner_id` onto every child — a schema change
plus a standing obligation to keep it in step, on exactly the tables whose scoping
is easiest to get wrong — or to hand-write the predicate, which is what
declarative scoping exists to replace.

Reads join the parent and apply the predicate. Updates and deletes read the row
back through it and answer `404` on a miss, exactly as they do for `ForceFilter`.

**Creates are scoped too, and they have to be.** The foreign key is what the whole
scope hangs from, and it's the one part of it the client supplies. On a create
there's no row to read back, so without a check nothing looks at `item_id` at all
and `POST {"item_id": "<another tenant's>"}` lands a row under their parent — where
they can see it and you can't. On an update the row read back is the one the *old*
`item_id` points at, so a `PATCH` that rewrites the key passes a check of where the
row used to be and then moves it somewhere else. So the parent a write names is
read through the scope's own predicate first, and a miss is the same `404` a scoped
read of that parent gives. A create that names no parent at all is refused with
`422`: the join would find nothing, so the row would be invisible to whoever
created it.

That parent read is the only added cost, and only a scope that runs through a
parent pays it.

There's no `TenancyVia`, because `Tenancy` is `ForceFilter` plus stamping the
tenant column onto writes — and the whole premise here is a model with no such
column to stamp. Checking the parent is what takes its place.

> **Actions:** `ForceFilterVia` registers on the DB step, so like every other
> DB-step middleware it doesn't run for a custom `Action` — see
> [Scoping Actions](#scoping-actions-tenancyaction--forcefilteraction). There's no
> `ForceFilterViaAction`: an `ActionScope`'s filters apply to whatever model the
> handler touches, while a `Via` scope is resolved against one specific model's
> relations, and an action runs on a synthetic model with none. For a single-model
> action, build the nested `FilterExpr` by hand (`IsNested`, `RelationKey`,
> `RelationTable`, `RelationFK`, `NestedField`, `Forced`) and pass it to
> `ctx.SetActionScope`. `ctx.ViaFilter` — the resolver `ForceFilterVia` is built
> on — reports this rather than guessing if you call it from an action.

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

Row-level scopers (`ForceFilter`, `ForceFilterVia`, `Tenancy`) must run **Before**
the default DB step — the default position — so the filter is in place when the
SELECT or UPDATE runs. Post-write hooks must run **After**, so they observe the result.
The package's defaults follow this; only change them if you know why.
