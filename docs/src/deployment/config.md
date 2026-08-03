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
| `PathPrefix` | `/api` | URL prefix prepended to generated model and documentation routes |
| `Documentation` | zero value (unmounted) | explicitly publish generated OpenAPI/AsyncAPI documents or protect both with shared middleware |
| `ServiceName` | `""` | service identifier added to logs, audit records, and the `X-Service-Name` response header |
| `StaticDir` | `""` | filesystem directory served as static files; empty serves nothing (opt-in). Relative paths resolve against cwd |
| `StaticPrefix` | `/static` | URL prefix the static directory is mounted under, at the router root |
| `StaticDisabled` | `false` | turn static file serving off even when `StaticDir` is set |
| `HTTPAccessControlled` | `false` | assert that non-empty `HTTPMiddlewares` protects every route for `ValidateProduction`; does not install auth |

`PathPrefix` does **not** affect `/static` or `/files`; those are mounted at the
router root. The probe endpoints — `/live`, `/ready`, and `/health` — do sit
under it, so the default prefix serves them at `/api/live`, `/api/ready`, and
`/api/health`. See [Static Files](../defining-your-api/static-files.md) for the
static serving options.

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

## Limits

| Field | Default | Purpose |
|---|---|---|
| `MaxConcurrentExports` | `4` | how many `GET /:model/export` requests may run at once, server-wide; negative disables the limit |
| `QueryLimits` | see below | bounds client-controlled URL query and aggregate complexity; `ModelConfig.QueryLimits` can override individual fields for one model |

An export holds its entire result set in memory until the last byte reaches the
client, so concurrency multiplies the largest allocation the server makes. The
per-model `MaxExportRows` bounds one export's rows but not the row width nor the
number in flight; this bounds the product. Requests over the limit are refused
immediately with `503 EXPORT_BUSY` and a `Retry-After`, not queued. See
[CSV / XLSX Export](../advanced-topics/export.md#concurrency-cap).

`QueryLimits` uses these safe defaults:

| Field | Default |
|---|---:|
| `MaxURLBytes` | 8 KiB |
| `MaxFilterClauses` / `MaxFilterGroups` / `MaxFiltersPerGroup` | 32 / 8 / 8 |
| `MaxSortFields` / `MaxSelectFields` / `MaxIncludes` | 8 / 64 / 8 |
| `MaxAggregateSelectFields` / `MaxAggregateGroupFields` | 16 / 8 |
| `MaxAggregateFilters` / `MaxAggregateHaving` / `MaxAggregateSortFields` | 32 / 16 / 8 |
| `DefaultAggregateRows` / `MaxAggregateRows` | 100 / 200 |

A zero field inherits the default (or the global value in a per-model
override); a negative field disables that individual limit. The global
`MaxURLBytes` remains a router-level hard ceiling for every route. Oversized
URIs return `414 URI_TOO_LONG`; invalid list-query shapes return
`400 INVALID_QUERY`.

## Database

| Field | Default | Purpose |
|---|---|---|
| `DB` | nil | the default `DBAdapter`. Usually set via `server.SetDB(db)` after `MustRegister`. Optional when every model has its own `ModelConfig.Adapter` — see [Per-model adapter routing](databases.md#per-model-adapter-routing) |
| `DisableAutoMigrate` | `false` | skip schema migration on startup (migration runs by default) |
| `DBWriteURL` | `""` | DSN for the primary database (informational; populated by `ConfigFromEnv`) |
| `DBReadURL` | `""` | DSN for the read replica (informational) |
| `QueryTimeout` | `0` (unlimited) | per-request deadline applied to all DB calls; exceeding it produces `504 TIMEOUT` |

See [Database Backends](databases.md) for adapter construction.

`SetDB`, `SetStorage`, and `SetKeyProvider` support two-step initialization, but
they are configuration methods rather than runtime rotation APIs. Call them
before `Handler`, `Start`, `StartServices`, or `MigrateOnly`; those entry points
seal the server, and a later setter call panics without changing either the
configured or active backend. Construct a new server when these dependencies
must change.

## File storage and encryption

| Field | Purpose |
|---|---|
| `FilesConfig.Storage` | `maniflex.FileStorage` implementation for `mfx:"file"` fields and the `/files` endpoints. Required if any model uses file uploads. See [File Fields & Uploads](../defining-your-api/files.md). |
| `FilesConfig.BeforeMiddlewares` | `[]maniflex.MiddlewareFunc` wrapping the standalone `/files` endpoints. Empty = no auth (backward-compatible default); production deployments should populate this with at least an auth middleware. See [File Fields & Uploads](../defining-your-api/files.md#standalone-file-endpoints). |
| `FilesConfig.AllowPublic` | explicit declaration that mounted standalone `/files` routes are intentionally public; used by `ValidateProduction` and strict startup validation |
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

`Enabled` expands into the three standard flags *unless* one of those three is
already set, in which case it stays out of the way and you get exactly what you
named. `Bodies` and `Skips` are additive: setting one does not suppress the
expansion, so the example above gives all four.

```go
cfg.Trace = maniflex.PipelineTrace{Enabled: true, Steps: true}  // Steps only
cfg.Trace = maniflex.PipelineTrace{Bodies: true}                // Bodies only
```

Leave `Bodies` off in production.

## Lifecycle

| Field | Default | Purpose |
|---|---|---|
| `ShutdownTimeout` | `30s` | maximum time `Start()` waits for in-flight requests to finish on `SIGINT` / `SIGTERM` before forcing the listener closed |

See [Graceful Shutdown](shutdown.md).

## Probes

Three endpoints are mounted under `PathPrefix`. The first two have fixed
meanings and need no configuration:

| Endpoint | Answers | Behaviour |
|---|---|---|
| `GET {prefix}/live` | is this process alive? | always `200 {"status":"ok"}`. No I/O, no dependency, no lifecycle coupling — including throughout the drain |
| `GET {prefix}/ready` | should this process receive traffic? | `503` while starting or stopping, otherwise the result of every dependency check |
| `GET {prefix}/health` | *(legacy)* | meaning follows `HealthCheckDB`; kept as a compatibility alias |

Point `livenessProbe` at `/live` and `readinessProbe` at `/ready`. Pointing both
at `/health` is what the split exists to end: with `HealthCheckDB` on, an
unreachable database answers `503` to *both*, so Kubernetes restarts a process
whose only problem is a dependency it cannot fix by dying.

`/ready` reports the lifecycle first, without touching a dependency:

```json
503 {"status":"starting"}
503 {"status":"stopping"}
```

`stopping` appears the moment shutdown begins, which is what deregisters the pod
from its load balancer while in-flight requests drain. Otherwise every
dependency is checked concurrently:

```json
200 {"status":"ok"}
503 {"status":"not_ready"}
```

The per-dependency results are withheld by default, because the map names every
`ReadinessChecks` entry and says which of them are failing — telling anyone who
can reach the probe what your application is built on, and when it is degraded.
Set `Probes.PublishReadinessChecks` where the probe is reachable only from
inside the cluster, or alongside a `Probes.Ready.Middleware` that says who may
read it:

```json
200 {"status":"ok",        "checks":{"db":"ok","broker":"ok"}}
503 {"status":"not_ready", "checks":{"db":"ok","broker":"error"}}
```

Withholding them costs the orchestrator nothing — the status code is the whole
of its contract — and a failing check is logged through `Config.Logger` either
way, so a `503` is never undiagnosable. `GET {prefix}/health` is unaffected: its
`db` key is a name the framework owns rather than one you chose, so it describes
no topology.

A check reads `unknown` — which is not a failure — when the framework has no way
to test it: an adapter that does not implement `Ping`, or no adapter configured
at all.

Concurrent probe requests share one run of the checks, so a flood of probes
cannot be amplified into a flood against the dependencies they report on. It
coalesces rather than caches: a request arriving after the run finished starts a
new one, since readiness that is even slightly stale keeps a pod in the load
balancer after its database has gone away.

| Field | Default | Purpose |
|---|---|---|
| `ReadinessChecks` | none | the application's own dependency probes, reported by `/ready` beside the built-in `db` check |
| `HealthTimeout` | `3s` | budget shared by all dependency checks on `/ready`, and by `/health` when `HealthCheckDB` is on |
| `HealthCheckDB` | `false` | when true, `GET /health` pings every distinct registered adapter (Config.DB plus any per-model overrides) and returns `503` on failure. Governs `/health` alone — `/ready` always checks the database |

Set `HealthTimeout` shorter than your probe's `timeoutSeconds` so the handler
can return `503` cleanly before the probe times out.

Add a dependency of your own with `ReadinessChecks`:

```go
cfg := maniflex.Config{
    ReadinessChecks: []maniflex.ReadinessCheck{{
        Name:  "broker",
        Check: func(ctx context.Context) error { return broker.Ping(ctx) },
    }},
}
```

Checks run on every readiness request, so keep them to a pool or connection
probe rather than a full round-trip, and honour the `ctx`. A check that returns
an error — or panics, which is recovered — makes `/ready` answer `503`. The
error text is logged through `Config.Logger` and never written to the response:
a probe body is the one place an unauthenticated client reads straight from a
dependency, and connection strings live in those messages.

Names must be non-empty, unique, and not `db`, which the framework reserves;
anything else panics when the router is built rather than on a probe request.

`live`, `ready`, and `health` are reserved path segments under `PathPrefix` — a
custom action or route registered at one of them collides with the probe
already mounted there.

### Gating and unmounting the probes

The probes are mounted straight onto the router and never enter the model
pipeline, so **no `Pipeline.Auth` middleware runs for them** and
`authx.AllowPublic` has nothing to exempt them from. That is deliberate: an
orchestrator's probe is the canonical unauthenticated request.

`Config.Probes` is the override — the only lever scoped to the probes alone.
(`Config.HTTPMiddlewares` reaches them too, but it reaches every other route at
the same time.) Its zero value mounts all three publicly.

```go
cfg.Probes = maniflex.ProbesConfig{
    // Readiness names your dependencies and reports which are down.
    Ready: maniflex.ProbeConfig{
        Middleware: []maniflex.HTTPMiddleware{probeToken},
    },
    // The legacy endpoint, retired in favour of /live and /ready.
    Health: maniflex.ProbeConfig{Disabled: true},
}
```

| Field | Purpose |
|---|---|
| `Probes.Middleware` | wraps every mounted probe, in order |
| `Probes.{Live,Ready,Health}.Middleware` | wraps that one probe, **after** the shared chain — appended, not instead of it |
| `Probes.{Live,Ready,Health}.Disabled` | leaves that probe off the router entirely |
| `Probes.PublishReadinessChecks` | writes the per-dependency results into the `/ready` body; off by default |

A disabled probe is not mounted, so the request gets the router's plain `404`
and neither middleware chain runs. That is a more honest answer than a handler
that refuses: a `401` says the endpoint is there.

`AdaptAuth` reuses pipeline auth middleware here, the same way it does for
`Documentation.Middleware`:

```go
cfg.Probes.Ready.Middleware = []maniflex.HTTPMiddleware{
    maniflex.AdaptAuth(auth.JWTAuth(opts), auth.RequireRole("ops")),
}
```

> **Think before gating `/live`.** A liveness probe that receives a `401` is a
> liveness probe that fails, and Kubernetes answers a failing liveness probe by
> killing the container — during the graceful drain, taking its in-flight
> requests with it. Gating `/ready` alone is usually what you want.
>
> If you do gate `/live`, the probe has to carry the credential, and a kubelet
> `httpGet` varies a path far more easily than it rotates a header:
>
> ```yaml
> livenessProbe:
>   httpGet:
>     path: /api/live?token=...
>     port: 8080
> ```
>
> Which means the middleware must accept the query parameter, not only a header.

A public `/ready` is a defensible default, and the two things that used to make
it uncomfortable are handled without gating: the dependency names are withheld
unless you ask for them, and concurrent requests share one run of the checks.
Gate it when the endpoint is reachable from outside the cluster, or when even
`{"status":"not_ready"}` is more than you want to publish.

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
