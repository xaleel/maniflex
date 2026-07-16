# Configuration

`maniflex.Config` is the single struct passed to `maniflex.New`. Every field has a
sensible default; populate only the ones that differ from those defaults.

```go
server := maniflex.New(maniflex.Config{
    Port:        8080,
    PathPrefix:  "/api",
})
```

## Server

| Field | Default | Purpose |
|---|---|---|
| `Port` | `8080` | TCP port the HTTP server binds to |
| `PathPrefix` | `/api` | URL prefix prepended to every generated model route and `/openapi.json` |
| `ServiceName` | `""` | service identifier added to logs, audit records, and the `X-Service-Name` response header |
| `StaticDir` | `""` | filesystem directory served as static files; empty serves nothing (opt-in). Relative paths resolve against cwd |
| `StaticPrefix` | `/static` | URL prefix the static directory is mounted under, at the router root |
| `StaticDisabled` | `false` | turn static file serving off even when `StaticDir` is set |

`PathPrefix` does **not** affect `/static`, `/files`, or `/health`. Those are
mounted at the router root. See [Static Files](../defining-your-api/static-files.md) for the static
serving options.

## HTTP timeouts

The framework owns the `http.Server`, and `net/http` gives that struct no
deadlines by default. These are the ones it sets on your behalf:

| Field | Default | Purpose |
|---|---|---|
| `ReadHeaderTimeout` | `10s` | how long a connection may take to send its request headers |
| `IdleTimeout` | `120s` | how long a keep-alive connection may sit idle between requests |
| `ReadTimeout` | `0` (unbounded) | time to read an entire request, headers **and body** |
| `WriteTimeout` | `0` (unbounded) | time to write a response |

`ReadHeaderTimeout` is the slowloris defence. Without it a client can hold a
connection open forever by dribbling one header byte at a time, and enough such
connections exhaust the server's file descriptors without a single request ever
reaching the pipeline. Set a **negative** value to disable a timeout (which is
what `net/http` reads as "no deadline") — only sensible behind a proxy that
already bounds header reads.

`ReadTimeout` and `WriteTimeout` are deliberately left unset, because both are
whole-request deadlines rather than idle deadlines:

- A `ReadTimeout` caps how long a client may take to *upload*, so a large file
  over a slow link is severed mid-transfer.
- A `WriteTimeout` covers the entire response, so any value at all would cut a
  long-lived stream — `realtime.SSEHandler`, a large download — off at that mark.

Set them when you know your request sizes and have no streaming endpoints. The
header phase stays bounded by `ReadHeaderTimeout` either way.

## Database

| Field | Default | Purpose |
|---|---|---|
| `DB` | nil | the default `DBAdapter`. Usually set via `server.SetDB(db)` after `MustRegister`. Optional when every model has its own `ModelConfig.Adapter` — see [Per-model adapter routing](databases.md#per-model-adapter-routing) |
| `DisableAutoMigrate` | `false` | skip schema migration on startup (migration runs by default) |
| `DBWriteURL` | `""` | DSN for the primary database (informational; populated by `ConfigFromEnv`) |
| `DBReadURL` | `""` | DSN for the read replica (informational) |
| `QueryTimeout` | `0` (unlimited) | per-request deadline applied to all DB calls; exceeding it produces `504 TIMEOUT` |

See [Database Backends](databases.md) for adapter construction.

## File storage and encryption

| Field | Purpose |
|---|---|
| `FileStorage` | `maniflex.FileStorage` implementation for `mfx:"file"` fields and the `/files` endpoints. Required if any model uses file uploads. See [File Fields & Uploads](../defining-your-api/files.md). |
| `FileMiddleware` | `[]maniflex.MiddlewareFunc` wrapping the standalone `/files` endpoints. Empty = no auth (backward-compatible default); production deployments should populate this with at least an auth middleware. See [File Fields & Uploads](../defining-your-api/files.md#standalone-file-endpoints). |
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

`maniflex.ConfigFromEnv(prefix)` populates a `Config` from a conventional set of
environment variables. Use it for twelve-factor deployments, then override
individual fields in code where needed.

```go
cfg, err := maniflex.ConfigFromEnv("")   // or "ORDERS" → ORDERS_PORT, ORDERS_DB_WRITE_URL, …
if err != nil {
    log.Fatal(err)
}
cfg.DisableAutoMigrate = true  // disable for production
server := maniflex.New(cfg)
```

These are the variables it reads, and the only ones — anything else on `Config`
is set in code:

| Variable              | Field             | Value                                     |
| --------------------- | ----------------- | ----------------------------------------- |
| `PORT`                | `Port`            | integer, 1–65535                          |
| `DB_WRITE_URL`        | `DBWriteURL`      | string                                    |
| `DB_READ_URL`         | `DBReadURL`       | string                                    |
| `QUERY_TIMEOUT_MS`    | `QueryTimeout`    | positive integer, milliseconds            |
| `SHUTDOWN_TIMEOUT_S`  | `ShutdownTimeout` | positive integer, seconds                 |
| `SERVICE_NAME`        | `ServiceName`     | string                                    |
| `HEALTH_CHECK_DB`     | `HealthCheckDB`   | `true`/`false`, `1`/`0`, `yes`/`no`, `on`/`off` |

A variable that is **unset** leaves its field at the zero value, for
`ApplyDefaults` to fill in. A variable that is **set but unreadable** is an
error — `PORT=808O`, `QUERY_TIMEOUT_MS=abc`, `HEALTH_CHECK_DB=ture` — naming the
variable and the value it could not read. Every bad variable is reported at once,
so two typos take one deploy to find rather than two. Don't discard this error: a
mistyped `PORT` that is quietly ignored gives you a healthy-looking server
listening on 8080, and nothing anywhere says why.
