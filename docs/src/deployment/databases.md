# Database Backends

maniflex ships two database adapters, both built on `database/sql` and sharing a
single SQL core (`db/sqlcore`). They expose the same interface; switching
between them is one import line.

| Adapter | Module | Driver |
|---|---|---|
| SQLite | `maniflex/db/sqlite` | `modernc.org/sqlite` — pure Go, no CGo |
| PostgreSQL | `maniflex/db/postgres` | `github.com/lib/pq` |

Each adapter lives in its own Go module so a project only pulls in the driver
it actually uses.

## SQLite

The default choice for development, tests, and small deployments. The pure-Go
driver means no CGo and no external service — `go run .` is enough to start a
local server with a working database.

```go
import "github.com/xaleel/maniflex/db/sqlite"

db, err := sqlite.Open("./app.db", server.Registry())
if err != nil {
    log.Fatal(err)
}
defer db.Close()
server.SetDB(db)
```

Common DSNs:

| DSN | Effect |
|---|---|
| `./app.db` | persistent file in the working directory |
| `:memory:` | per-process in-memory database; vanishes on shutdown |
| `file:./app.db?_txlock=immediate` | upgrade write locks to immediate — required for `LockForUpdate` to behave like Postgres |

SQLite is single-writer by design. The framework serialises writes through one
connection internally; reads run on a pool. This is plenty for most internal
tools and many production APIs.

## PostgreSQL

The recommended adapter for any multi-process deployment. It supports
genuine concurrent writers, real `FOR UPDATE` locks, and read replicas.

```go
import "github.com/xaleel/maniflex/db/postgres"

// Open(writeDSN, readDSN, registry) — positional arguments.
db, err := postgres.Open(
    "postgres://user:pass@host/db?sslmode=require",      // write DSN
    "postgres://user:pass@read.host/db?sslmode=require", // read DSN (optional)
    server.Registry(),
)
if err != nil {
    log.Fatal(err)
}
defer db.Close()
server.SetDB(db)
```

Pass an empty read DSN (`""`) to route reads to the primary. The adapter selects
the appropriate pool per request based on the operation — `OpList` and `OpRead`
go to the read pool, everything else to the write pool.

For connection-pool and session tuning use `postgres.OpenWithConfig(writeDSN,
readDSN, registry, writePool, readPool, session)` — see
[PostgreSQL in Production](../advanced-topics/postgres.md) for replica lag
handling and SSL.

## Switching between them

The adapter is the only thing that changes; nothing else in the application
needs to know which database is in use:

```diff
- import "github.com/xaleel/maniflex/db/sqlite"
+ import "github.com/xaleel/maniflex/db/postgres"

- db, err := sqlite.Open("./app.db", server.Registry())
+ db, err := postgres.Open(os.Getenv("DB_URL"), "", server.Registry())
```

Models, middleware, and queries are portable across both backends because they
go through `database/sql` + the shared `db/sqlcore` adapter. Migrations
emitted by `AutoMigrate` use a portable subset of SQL.

## AutoMigrate

Unless `Config.DisableAutoMigrate` is set (migration runs by default), the adapter:

1. Creates any table that does not yet exist for a registered model.
2. Adds any column that exists on the struct but not in the table.
3. Logs a warning for columns that exist in the table but not on the struct
   (the framework never drops columns automatically).
4. Creates indexes declared in `ModelConfig.Indices` or auto-generated for
   `mfx:"scheduled"` fields.

`AutoMigrate` is suitable for development and many small deployments. For
larger systems, set `DisableAutoMigrate: true` and manage the schema with a
dedicated migration tool.

## The `DBAdapter` interface

Both shipped adapters implement `maniflex.DBAdapter`. Custom backends — an HTTP
data service, a remote API, a different SQL database — implement the same
interface and inject the result through `server.SetDB(myAdapter)`. The
interface is in [db.go](../../../db.go).

## Per-model adapter routing

`Config.DB` sets the default adapter. Individual models can override it by
passing `ModelConfig.Adapter`:

```go
ordersDB, _    := postgres.Open(ordersDSN, "", server.Registry())
inventoryDB, _ := postgres.Open(inventoryDSN, "", server.Registry())

server.MustRegister(
    Order{},         maniflex.ModelConfig{Adapter: ordersDB},
    InventoryItem{}, maniflex.ModelConfig{Adapter: inventoryDB},
    User{},          // unrouted — falls back to Config.DB
)
```

The framework treats each distinct adapter as its own database:

- **AutoMigrate** runs once per adapter, with a filtered registry view so each
  adapter only sees the models routed to it. Tables for `Order` are never
  created on the inventory DB and vice-versa.
- **CRUD requests** (`GET /orders`, `POST /orders`) route through
  `Order.Adapter`. The DB step picks the per-model adapter automatically.
- **`ctx.BeginTx` / `ctx.RawQuery` / `ctx.RawExec`** use the request's model
  adapter, so middleware and custom actions stay on the right DB.
- **`ctx.GetModel("OtherModel")`** uses the *target* model's adapter — handy
  for cross-DB reads — but it cannot share a transaction across adapters: if
  `ctx.Tx` was opened on `dbA` and you call `GetModel("X")` where `X` lives
  on `dbB`, the accessor falls back to a non-transactional read against `dbB`.

`Config.DB` is optional when every registered model has its own `Adapter`.
The server starts cleanly with `DB: nil` and routes everything through the
per-model overrides. If any model is unrouted and `Config.DB` is also nil,
startup fails with a clear error naming the unrouted models.

### Constraint: transactions are adapter-scoped

A single database transaction cannot span two adapters. Two consequences:

1. `maniflex.Batch` rejects a `b.Create("X", ...)` call where `X` routes to a
   different adapter than the batch transaction was opened on. The error
   message points to `pkg/saga` as the cross-adapter pattern.
2. Manually-opened `ctx.Tx` only protects writes against the request's own
   model adapter. Cross-adapter writes through `ctx.GetModel(...)` happen
   outside that transaction.

For coordinated writes across databases, use [`pkg/saga`](../defining-your-api/models.md) —
compensating transactions are the supported pattern.

## Choosing

| Need | Pick |
|---|---|
| Quick start, tests, small single-process services | SQLite |
| Multi-process deployment, real concurrency, replicas | PostgreSQL |
| Both (the codebase will outgrow SQLite) | SQLite locally, Postgres in production — same code |
