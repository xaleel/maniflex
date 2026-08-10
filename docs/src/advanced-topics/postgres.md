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
    postgres.PoolConfig{MaxOpenConns: 5},  // write pool
    postgres.PoolConfig{MaxOpenConns: 15}, // read pool
    postgres.SessionConfig{
        StatementTimeout: 10 * time.Second,
        ApplicationName:  "orders-api",
        SchemaName:       &schema, // search_path; auto-created on connect if absent
    },
)
```

## Connection-pool tuning

The defaults are sized for the smallest tier a managed provider sells.
`OpenWithConfig` exposes them as `PoolConfig` fields, set independently for the
write and read pools:

| `PoolConfig` field | Default (write / read) | Considerations |
|---|---|---|
| `MaxOpenConns` | 3 / 6 | `(write + read) × processes ≤ max_connections − reserved` |
| `MaxIdleConns` | equal to `MaxOpenConns` | a pool this small keeps every connection; reopening costs a TLS handshake plus the session `SET` round trip |
| `ConnMaxLifetime` | 30 min | rotate connections to pick up failover or DNS changes |
| `ConnMaxIdleTime` | 5 min | release connections an idle process is no longer using |

### Sizing against your server

`Open` builds both pools even when the read DSN is empty, so the number that
matters is the **sum**, multiplied by every process that connects — an API, a
worker, a migration job, each replica. Providers also reserve connections for
themselves, and you want a slot left for `psql`:

| Provider (entry tier) | `max_connections` | Usable |
|---|---|---|
| Heroku Postgres Essential-0/1/2 | 20 | 20 |
| DigitalOcean Managed PG (1 GiB) | 25 (25 per GiB) | 22 — 3 reserved |
| GCP Cloud SQL `db-f1-micro` | 25 | ~22 |
| Azure Flexible Server B1ms | 50 | 35 — 15 reserved |
| Supabase Nano / Micro | 60 | 60 |
| Neon 0.25 CU | 104 | 97 — 7 reserved |
| AWS RDS `db.t4g.micro` | ~112 (from instance memory) | ~110 |

At `Open`, the adapter reads the server's own `max_connections` and logs a
`WARN` when the pools claim more than half of it. Set `SessionConfig.Logger` to
route that warning into your application's logger; it defaults to
`slog.Default()`. The check never fails startup.

Raising the ceiling past what your instance needs makes things slower, not
faster: an entry-tier instance has one or two vCPUs, and Postgres throughput
peaks at a low multiple of core count. Extra connections buy queueing headroom
for bursts. Watch `Stats().WaitCount` and `WaitDuration` on the pool — sustained
waiting is the signal to size up, and the read pool is almost always the one
that needs it.

If you front Postgres with **PgBouncer in transaction-pooling mode**:

- Set `MaxOpenConns` on the client to roughly match the bouncer's
  `default_pool_size`.
- **Add `binary_parameters=yes` to the DSN.** `lib/pq` sends any parameterised
  query as Parse/Describe/`Sync` followed by Bind/Execute/`Sync` — two implicit
  transactions, so the bouncer may hand the server connection to another client
  in between and the unnamed prepared statement is gone when the Bind lands.
  The symptom is intermittent `prepared statement "" does not exist` under load.
  With `binary_parameters=yes`, `lib/pq` sends Parse/Bind/Describe/Execute/Sync
  in one packet, which is a single implicit transaction and safe. The framework
  caches no *named* statements of its own; this is the driver's behaviour and
  applies to every query, including your raw ones.
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

Each schema is migrated independently and gets its own constraints, so foreign
keys and their `onDelete` actions apply in every one. Before v0.3.2 they did
not: the check for an existing constraint was not scoped to a schema, and
constraint names are derived from table and column, so the same model in a
second schema looked like it had already been migrated. Whichever schema ran
`AutoMigrate` first got its foreign keys and every later one silently got none —
losing referential integrity and `cascade`/`restrict`/`setNull` with them. If
you ran a multi-schema deployment on an earlier version, verify the constraints
are present before relying on them:

```sql
SELECT table_schema, table_name, constraint_name
FROM information_schema.table_constraints
WHERE constraint_type = 'FOREIGN KEY'
ORDER BY table_schema, table_name;
```

Re-running `AutoMigrate` on the affected schemas adds what is missing.

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
server := maniflex.New(maniflex.Config{
    DisableAutoMigrate: true,
    // other application settings
})
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
  the `response.Metrics` request observer or a separate collector.
- Set `Config.QueryTimeout` to bound slow queries; offending requests return
  `504 TIMEOUT` rather than holding a connection open.
- Postgres logs (`log_min_duration_statement`) and `pg_stat_statements` are
  the canonical way to identify slow queries; the framework does not duplicate
  that.
