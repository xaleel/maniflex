# Configuration

`maniflex.Config` is the single struct passed to `maniflex.New`. Every field has a
sensible default; populate only the ones that differ from those defaults.

```go
server := maniflex.New(maniflex.Config{
    Port:        8080,
    PathPrefix:  "/api",
    AutoMigrate: true,
})
```

## Server

| Field | Default | Purpose |
|---|---|---|
| `Port` | `8080` | TCP port the HTTP server binds to |
| `PathPrefix` | `/api` | URL prefix prepended to every generated model route and `/openapi.json` |
| `ServiceName` | `""` | service identifier added to logs, audit records, and the `X-Service-Name` response header |
| `StaticDir` | `<cwd>/static` | filesystem directory served as static files (relative paths resolve against cwd) |
| `StaticPrefix` | `/static` | URL prefix the static directory is mounted under, at the router root |
| `StaticDisabled` | `false` | turn static file serving off entirely, even when the directory exists |

`PathPrefix` does **not** affect `/static`, `/files`, or `/health`. Those are
mounted at the router root. See [Static Files](static-files.md) for the static
serving options.

## Database

| Field | Default | Purpose |
|---|---|---|
| `DB` | nil | the default `DBAdapter`. Usually set via `server.SetDB(db)` after `MustRegister`. Optional when every model has its own `ModelConfig.Adapter` — see [Per-model adapter routing](databases.md#per-model-adapter-routing) |
| `AutoMigrate` | `true` | run schema migration on startup |
| `DBWriteURL` | `""` | DSN for the primary database (informational; populated by `ConfigFromEnv`) |
| `DBReadURL` | `""` | DSN for the read replica (informational) |
| `QueryTimeout` | `0` (unlimited) | per-request deadline applied to all DB calls; exceeding it produces `504 TIMEOUT` |

See [Database Backends](databases.md) for adapter construction.

## File storage and encryption

| Field | Purpose |
|---|---|
| `FileStorage` | `maniflex.FileStorage` implementation for `mfx:"file"` fields and the `/files` endpoints. Required if any model uses file uploads. See [File Fields & Uploads](files.md). |
| `FileMiddleware` | `[]maniflex.MiddlewareFunc` wrapping the standalone `/files` endpoints. Empty = no auth (backward-compatible default); production deployments should populate this with at least an auth middleware. See [File Fields & Uploads](files.md#standalone-file-endpoints). |
| `KeyProvider` | `maniflex.KeyProvider` for `mfx:"encrypted"` fields. Without one, encrypted fields refuse writes with `500 ENCRYPTION_NOT_CONFIGURED`. |

## Logging

| Field | Default | Purpose |
|---|---|---|
| `Logger` | `slog.Default()` | logger used for lifecycle, per-request, and adapter messages |
| `PanicLogger` | falls back to `Logger` | sink for the panic recoverer's structured panic records |
| `Trace` | zero (off) | pipeline tracing — see below |

`Logger` is used by `ctx.Logger()`, which adds `request_id`, `trace_id`, and
`service` attributes per request. Route it to a JSON handler in production.

## Pipeline tracing

`Config.Trace` enables verbose debug output of the request pipeline. All trace
output is at `DEBUG` level through `Logger`, so the handler must accept
`DEBUG` records to see anything.

| Sub-flag | Effect |
|---|---|
| `Enabled` | shorthand for `Steps + Timings + Aborts` |
| `Steps` | enter/exit record per middleware |
| `Timings` | per-middleware elapsed time on exit records |
| `Aborts` | the source file:line of every `ctx.Abort` call |
| `Bodies` | log field names present in `ctx.ParsedBody` (opt-in; may expose sensitive field names) |
| `Skips` | log middleware skipped by `ForModel`/`ForOperation` filters |

```go
cfg.Trace = maniflex.PipelineTrace{Enabled: true, Skips: true}
```

Leave `Bodies` off in production.

## Lifecycle

| Field | Default | Purpose |
|---|---|---|
| `ShutdownTimeout` | `30s` | maximum time `Start()` waits for in-flight requests to finish on `SIGINT` / `SIGTERM` before forcing the listener closed |

See [Graceful Shutdown](shutdown.md).

## Health probe

| Field | Default | Purpose |
|---|---|---|
| `HealthCheckDB` | `false` | when true, `GET /health` pings every distinct registered adapter (Config.DB plus any per-model overrides) and returns `503` on failure. Driver error messages are logged, not echoed in the response body, so DSN fragments can't leak. |
| `HealthTimeout` | `3s` | maximum time the health handler waits for the DB ping |

Set `HealthTimeout` shorter than your probe's `timeoutSeconds` so the handler
can return `503` cleanly before the probe times out.

## Reading from environment

`maniflex.ConfigFromEnv()` populates a `Config` from a conventional set of
environment variables (`PORT`, `PATH_PREFIX`, `DB_WRITE_URL`, `DB_READ_URL`,
`SERVICE_NAME`, `LOG_LEVEL`, …). Use it for twelve-factor deployments, then
override individual fields in code where needed.

```go
cfg := maniflex.ConfigFromEnv()
cfg.AutoMigrate = false  // disable for production
server := maniflex.New(cfg)
```
