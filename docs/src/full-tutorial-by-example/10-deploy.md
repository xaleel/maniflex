# 10. Deploying to Production

The bookstore runs cleanly on SQLite + `go run .` for development. Production
needs three additional things: a real database, configuration from the
environment, and a sensible operational contract — health probes,
structured logs, graceful shutdown. Code changes are minimal; the framework
was already built for this.

## Swapping SQLite for PostgreSQL

Add the satellite:

```bash
go get github.com/xaleel/maniflex/db/postgres
```

Change one import in `main.go`:

```diff
- import "github.com/xaleel/maniflex/db/sqlite"
+ import "github.com/xaleel/maniflex/db/postgres"
```

…and the adapter open call:

```go
// Open(writeDSN, readDSN, registry) is positional. For pool/session tuning use
// OpenWithConfig(writeDSN, readDSN, registry, writePool, readPool, session).
db, err := postgres.OpenWithConfig(
    os.Getenv("DB_WRITE_URL"),
    os.Getenv("DB_READ_URL"), // optional; "" routes reads to the primary
    server.Registry(),
    postgres.PoolConfig{MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 30 * time.Minute}, // write pool
    postgres.PoolConfig{MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 30 * time.Minute}, // read pool
    postgres.SessionConfig{ApplicationName: "bookstore"},
)
```

Models, middleware, actions, and tests all carry over unchanged. The shared
`db/sqlcore` adapter means SQL emitted by `AutoMigrate` is portable between
the two backends. See [PostgreSQL in Production](../advanced-topics/postgres.md) for
pool tuning, read replicas, and migration choices.

## Configuration from environment

A single `Config` populated from `os.Getenv`:

```go
// config.go
func loadConfig() maniflex.Config {
    return maniflex.Config{
        Port:        envInt("PORT", 8080),
        PathPrefix:  envStr("PATH_PREFIX", "/api"),
        ServiceName: envStr("SERVICE_NAME", "bookstore"),

        DisableAutoMigrate: envStr("AUTO_MIGRATE", "true") != "true",
        QueryTimeout:    envDuration("QUERY_TIMEOUT", 30*time.Second),
        ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 30*time.Second),

        HealthCheckDB: true,
        HealthTimeout: 3 * time.Second,

        Logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
            Level: slog.LevelInfo,
        })),
    }
}
```

`maniflex` also ships a helper, `maniflex.ConfigFromEnv()`, that reads a conventional
set of environment variables (`PORT`, `PATH_PREFIX`, `DB_WRITE_URL`, …).
Pick whichever style fits your team — both produce the same `maniflex.Config`.

## Production-safe migrations

Migration runs by default (convenient in development). In production, the
prevailing pattern is:

1. Disable `AutoMigrate` on every instance.
2. Run schema changes through a dedicated migration tool
   (`golang-migrate`, Atlas, sqlc-migrate) executed as a separate one-shot
   step in your deploy pipeline.
3. Roll out the new application code afterwards.

```env
AUTO_MIGRATE=false
```

The framework's auto-migrator never drops columns, but it isn't aware of
your release strategy — splitting "deploy" and "migrate" into two steps
lets you stage them deliberately.

## Health probes

`HealthCheckDB: true` enables a real probe — `GET /health` calls
`db.Ping()` with a `HealthTimeout` budget. Kubernetes:

```yaml
livenessProbe:
    httpGet:
        path: /health
        port: 8080
    periodSeconds: 10
    timeoutSeconds: 5
readinessProbe:
    httpGet:
        path: /health
        port: 8080
    periodSeconds: 5
    timeoutSeconds: 5
```

Set `HealthTimeout` (default 3s) shorter than the probe's `timeoutSeconds`
so the handler can return a clean `503` before the probe gives up.

`terminationGracePeriodSeconds` on the pod should be **longer** than
`Config.ShutdownTimeout`, otherwise Kubernetes will `SIGKILL` the process
before in-flight requests have finished. With the defaults (30s shutdown),
60s grace is a comfortable buffer.

## Logging and tracing

The JSON handler above turns every line into a structured record that an
aggregator can index. `ctx.Logger()` automatically adds `request_id` and
`trace_id` per request, so a single trace can be reconstructed end-to-end.

Set `Config.ServiceName` so every log line and every audit record carries
the service identifier — invaluable when a single aggregator collects logs
from several services.

For a debugging spike, enable pipeline tracing:

```env
TRACE=1
```

```go
if envStr("TRACE", "") != "" {
    cfg.Trace = maniflex.PipelineTrace{Enabled: true, Skips: true}
}
```

`Steps`, `Timings`, and `Aborts` produce DEBUG-level records that show
every middleware enter/exit and the file:line of every `Abort` call.
Disable in normal operation — they are high-volume.

## The Dockerfile

A typical Dockerfile for the binary:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/bookstore ./

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/bookstore /bookstore
COPY static /static
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/bookstore"]
```

`CGO_ENABLED=0` works because `maniflex/db/postgres` uses `lib/pq` (pure Go) and
nothing else in the framework requires a C toolchain.

`static/` is copied so the Scalar OpenAPI viewer is reachable at
`/static/openapi.html`.

## Production checklist

|                                     | Setting                                                       |
| ----------------------------------- | ------------------------------------------------------------- |
| Database                            | `maniflex/db/postgres` with `WriteURL` and optional `ReadURL` |
| Migrations                          | `DisableAutoMigrate: true` + external migration tool                |
| Logger                              | JSON handler                                                  |
| `Config.ServiceName`                | the service name                                              |
| `Config.QueryTimeout`               | bounded (e.g. `30s`)                                          |
| `Config.ShutdownTimeout`            | matches the slowest legitimate request                        |
| `Config.HealthCheckDB`              | `true`                                                        |
| K8s `terminationGracePeriodSeconds` | larger than `ShutdownTimeout`                                 |
| TLS                                 | terminated at the load balancer                               |
| Auth                                | `auth.JWTAuth` with an asymmetric key from your IdP           |
| Rate limits                         | `db.RateLimit` on password-reset / sign-up / login            |
| Audit log                           | `db.AuditLog` on `OpCreate / OpUpdate / OpDelete`             |
| File storage                        | swap `LocalStorage` for S3 / R2 / GCS                         |
| Outbox worker                       | run alongside the API, or as a separate deployment            |

## Where to go from here

The tutorial finishes here. From this point, the reference pages cover
everything in more depth, and the code base is small enough to grow in any
direction:

- More middleware from the [Middleware Catalogue](../middleware-catalogue/index.md).
- Customisation via [Writing Middleware](../the-request-pipeline/middleware.md).
- Advanced workflows in [Custom Endpoints](../advanced-topics/actions.md),
  [Raw Queries & Query Models](../advanced-topics/raw-queries.md), and
  [Batch Operations & Sagas](../advanced-topics/batch-saga.md).
- Hardening with [Auth & Security Hardening](../advanced-topics/security.md).

The shape of `main.go` has not changed in ten parts. Add models, add
middleware, add actions — the wiring is the same.
