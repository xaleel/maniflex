# PostgreSQL in Production

The `maniflex/db/postgres` adapter is the recommended backend for any deployment
beyond a single process. This page collects production-relevant details that
go beyond the [Database Backends](../deployment/databases.md) overview.

## Opening the adapter

```go
import "github.com/xaleel/maniflex/db/postgres"

// Single primary (no replica) — pass "" for the read DSN.
db, err := postgres.Open(
    os.Getenv("DB_WRITE_URL"), // primary / write DSN (required)
    os.Getenv("DB_READ_URL"),  // replica / read DSN ("" → reuse the primary)
    server.Registry(),
)
if err != nil {
    log.Fatal(err)
}
server.SetDB(db)
```

`Open` takes the write DSN, the read DSN, and the registry. The write DSN is
required; pass `""` for the read DSN to route reads at the primary. Both are
standard libpq connection strings or URLs
(`postgres://user:pass@host:5432/dbname?sslmode=require`). `MustOpen` is the
panic-on-error variant for package-level initialisation.

### Tuning pools and session settings

`Open` applies production defaults. To override them, use `OpenWithConfig`,
which takes a separate `PoolConfig` for the write and read pools plus one
`SessionConfig`. Any zero-value field is replaced by the default `Open` uses, so
you set only what you want to change:

```go
schema := "orders"
db, err := postgres.OpenWithConfig(
    writeDSN, readDSN, server.Registry(),
    postgres.PoolConfig{MaxOpenConns: 20}, // write pool
    postgres.PoolConfig{MaxOpenConns: 60}, // read pool
    postgres.SessionConfig{
        StatementTimeout: 10 * time.Second,
        ApplicationName:  "orders-api",
        SchemaName:       &schema, // search_path; auto-created on connect if absent
    },
)
```

## Connection-pool tuning

The defaults suit a small service. `OpenWithConfig` exposes them as `PoolConfig`
fields, set independently for the write and read pools:

| `PoolConfig` field | Default (write / read) | Considerations |
|---|---|---|
| `MaxOpenConns` | 20 / 40 | keep the sum ≤ `max_connections / number_of_app_instances` |
| `MaxIdleConns` | half of `MaxOpenConns` | enough open to absorb bursts, not so many you waste server slots |
| `ConnMaxLifetime` | 30 min | rotate connections to pick up failover or DNS changes |
| `ConnMaxIdleTime` | 5 min | close idle connections so PgBouncer can recycle |

If you front Postgres with **PgBouncer in transaction-pooling mode**:

- Set `MaxOpenConns` on the client to roughly match the bouncer's
  `default_pool_size`.
- Avoid prepared statements that span transactions (the framework doesn't use
  any; your raw queries should follow suit).
- `LISTEN` / `NOTIFY` is not supported under transaction pooling — use the
  event-bus satellites instead.

## Session settings

`SessionConfig` carries session-level parameters the adapter re-applies (`SET …`)
on every new physical connection — Postgres does not persist them across
reconnects, so they must be set per connection:

| `SessionConfig` field | Default | Effect |
|---|---|---|
| `StatementTimeout` | 30s | cancels any statement that runs longer (`0` = server default) |
| `LockTimeout` | 5s | aborts a statement that waits too long for a lock |
| `IdleInTransactionTimeout` | 60s | aborts transactions left idle — guards against hung app code |
| `ApplicationName` | `maniflex` | shown in `pg_stat_activity` and server logs |
| `TimeZone` | `UTC` | session time zone for `TIMESTAMPTZ` rendering |
| `SchemaName` | `public` | schema set as `search_path` (see below) |

### Schema isolation (`search_path`)

By default the adapter operates in the `public` schema. Set
`SessionConfig.SchemaName` to scope every connection to a dedicated schema via
`SET search_path` — handy for multi-tenant deployments or co-locating several
apps in one database. The schema is **created on connect** when it does not yet
exist (`CREATE SCHEMA IF NOT EXISTS`), so `AutoMigrate` has somewhere to place
its tables; an existing schema is left untouched (a role with `USAGE` but not
`CREATE` still connects). The name must be a plain SQL identifier
(`[A-Za-z_][A-Za-z0-9_$]*`); `public` is assumed to always exist and is never
re-created.

## Read replicas

When a read DSN is supplied, `OpList` and `OpRead` operations are routed to the
read pool; everything else uses the write pool. Trade-offs:

- Reads inside an active write transaction route to the write pool, even when
  a read replica is configured — read-your-writes is preserved.
- Pure read endpoints get the replica's spare capacity without any code
  change.
- The application sees the replica's normal lag for non-transactional reads.
  If a workflow depends on read-your-writes outside a transaction, run it inside
  a write transaction so the read lands on the primary.

## `FOR UPDATE` and pessimistic locking

`ctx.LockForUpdate` translates to `SELECT … FOR UPDATE` on Postgres. The lock
is held until the enclosing transaction commits or rolls back. Typical use:

```go
row, err := ctx.LockForUpdate("StockBalance", stockID)
if err != nil {
    return err
}
if row["quantity"].(int64) < 1 {
    ctx.Abort(http.StatusConflict, "OUT_OF_STOCK", "no inventory")
    return nil
}
// safe to subtract — concurrent writers are blocked
```

Combine with `maniflex.WithTransaction` (or manual `BeginTx`) so the lock has a
transaction to scope it.

## Isolation levels

`maniflex.WithTransaction(&maniflex.TxOptions{Isolation: sql.LevelSerializable})` opens
the request in `SERIALIZABLE` isolation. Postgres serialisation failures
produce `40001` errors. Note that `NormalizeError` maps only the constraint
codes `23505` / `23502` / `23503` to `*maniflex.ErrConstraint`; a `40001`
serialisation failure is **not** normalised — it propagates as a generic error
and surfaces as a `500`. If you need transparent retry on serialisation
failures, detect the `40001` SQLSTATE yourself (e.g. in an action handler or a
custom middleware) and retry the transaction.

Most APIs do fine with the default `READ COMMITTED` plus `LockForUpdate` on
the contested rows; reach for `SERIALIZABLE` when the contention pattern is
more complex than a single row.

## AutoMigrate at scale

`AutoMigrate` is enabled by default. For larger production databases, prefer:

```go
server := maniflex.New(maniflex.Config{DisableAutoMigrate: true, ...})
```

…and run schema changes through a dedicated migration tool (sqlc-migrate,
golang-migrate, Atlas, etc.). The framework's auto-migrator is conservative —
it never drops columns and emits straightforward DDL — but coordinating
schema changes across replicas, dropped indexes, and rolling deploys is the
migration tool's job.

If you keep `AutoMigrate` enabled, run the first instance to completion
before scaling out; later instances will see all-up-to-date schema and skip
the work.

## TLS and connectivity

- Use `sslmode=require` (or stricter) on both the write and read DSN for any
  production connection. The driver respects the URL parameter.
- For Cloud SQL / RDS, the connection string is generated by the cloud
  console; copy it verbatim and store it as a secret.
- Resolve DNS lookups inside the process — don't pre-resolve at process
  start. The `ConnMaxLifetime` setting then picks up the new endpoint
  automatically during failover.

## Observability

- The adapter exposes pool statistics via `sql.DB.Stats()`; export them with
  the `response.Metrics` middleware or a separate collector.
- Set `Config.QueryTimeout` to bound slow queries; offending requests return
  `504 TIMEOUT` rather than holding a connection open.
- Postgres logs (`log_min_duration_statement`) and `pg_stat_statements` are
  the canonical way to identify slow queries; the framework does not duplicate
  that.
