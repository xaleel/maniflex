# Testing Applications

`github.com/xaleel/maniflex/maniflextest` is the supported integration-test
harness for Maniflex applications. It is a separate module because its SQLite
and PostgreSQL drivers should not become dependencies of the core framework.

## Starting a server

```go
server := maniflextest.New(t, maniflextest.Options{
    Config: maniflex.Config{PathPrefix: "/v1"},
    Models: []any{
        User{},
        maniflex.ModelConfig{TableName: "app_users"},
        Order{},
    },
    Setup: func(app *maniflex.Server) {
        registerMiddleware(app)
        registerActions(app)
    },
})
```

Setup runs after model registration and database injection but before route
validation and migration. `Config.DB` must remain nil; select the test adapter
with `Options.Database`.

By default, `New`:

1. opens a unique shared in-memory SQLite database;
2. installs support for request-scoped test principals;
3. invokes `Setup`;
4. validates and migrates the application;
5. starts an `httptest.Server`;
6. registers HTTP, lifecycle, database, and fixture cleanup with `t.Cleanup`.

`Options.StartServices` additionally starts registered services and lifecycle
hooks. `server.App()` exposes the application for background contexts and
direct assertions.

## Requests

`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, and `Do` resolve paths beneath
`Config.PathPrefix`. `DoRoot` targets a root-mounted endpoint such as a
standalone file route.

Request bodies may be `nil`, `[]byte`, an `io.Reader`, or any JSON-encodable
value. Request options include:

```go
maniflextest.Header("If-Match", etag)
maniflextest.Bearer(token)
maniflextest.As(principal)
```

A response exposes `StatusCode`, `Header`, and `Body`, plus `AssertStatus`,
`Decode`, `JSON`, `Data`, `DataList`, `ID`, and `ErrorCode`. Use
`DecodeData[T]` and `DecodeDataList[T]` to keep response assertions typed.

## Authentication

`Human(id, roles...)` and `ServiceAccount(id, scopes...)` construct common
principals. `As` carries one into the Auth pipeline, where the application's
authorization middleware sees it as `ctx.Auth`.

To test production authentication itself, set `DisableTestAuth: true` and send
the real credential with `Bearer` or `Header`.

## Fixtures

`Server.Seed` creates named fixtures through HTTP:

```go
records := server.Seed(
    maniflextest.Fixture{Name: "free", Path: "/plans", Body: freePlan},
    maniflextest.Fixture{Name: "pro", Path: "/plans", Body: proPlan},
)
proID := records.ID(t, "pro")
```

`Factory` builds repeated fixtures from an index. Seed preserves input order,
rejects duplicate names, requires every create to return `201`, and stores each
response's `data` object under its name.

## Time

Behaviour that turns on the passage of time — an idempotency replay window, a
rate-limit window, a cached read — used to be untestable from the outside: every
path to it measured against the wall clock, so asserting that a one-hour window
closes meant waiting an hour.

`maniflextest.NewClock` returns a clock that only moves when you say so.

```go
clock := maniflextest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
clock.Advance(90 * time.Minute)  // or clock.Set(someInstant)
```

Hand its `Now` method to whatever measures the time. The method value satisfies
`maniflex.Clock` and stays bound to the clock, so it can be passed anywhere one
is accepted:

| Where | What it governs |
| --- | --- |
| `maniflex.WithCacheClock(clock.Now)` on `NewMemoryCache` | idempotency replay windows, `db.CacheQuery` response caches, anything else backed by that `CacheStore` |
| `db.RateLimitConfig.Clock` | the in-process rate-limit window (no effect when `Backend` is set — there the window belongs to the backend) |
| `scheduled.Config.Clock` | when the `scheduled` runner considers a field's instant to have arrived |

A nil clock anywhere is the wall clock, so nothing changes for code that does
not set one.

The whole shape, from a test that runs in CI:

```go
{{#include ../../../maniflextest/expiry_test.go:expiry}}
```

**What it does not reach.** `created_at` and `updated_at` are stamped by the
database adapter, and job scheduling — `NotBefore`, lease expiry, cron ticks —
runs on its own wall clock. Neither takes an injected clock, so a test that
depends on either still has to work around it.

## Databases

- `SQLite()` is the default and creates a distinct in-memory database.
- `SQLite(path)` uses a file-backed database when persistence behaviour matters.
- `Postgres(dsn)` creates and later drops an isolated random schema.
- A custom `DatabaseFactory` can return any `maniflex.DBAdapter` plus an
  optional cleanup callback.

The harness owns adapters returned by its database factory and closes them
after cleanup. Do not share one adapter between concurrently running harness
servers.

## Middleware order

`Options.RecordPipeline` records which middleware runs for each request, and
`Server.PipelineSteps` reports it in execution order:

```go
server := maniflextest.New(t, maniflextest.Options{
	Models:         []any{Order{}},
	RecordPipeline: true,
	Setup: func(app *maniflex.Server) {
		app.Pipeline.Auth.Register(tenantGuard, maniflex.WithName("tenant-guard"))
	},
})

server.GET("/orders")

// [Auth/tenant-guard Auth/default Deserialize/default … DB/default Response/default]
steps := server.PipelineSteps()
```

Each entry is `Step/middleware`. A middleware registered without
[`WithName`](https://pkg.go.dev/github.com/xaleel/maniflex#WithName) is reported
as `[unnamed]`, and a step's built-in handler as `default`.

The framework reports order by logging one record per middleware, which the
harness captures. Matching those log records yourself is the thing to avoid: a
log message is not part of the API, so a test that greps for one can break on a
patch release. Recording wraps `Config.Logger` rather than replacing it, so a
logger you configured keeps receiving everything.

The report covers the most recent request. Requests issued concurrently against
one server share the recording, so assert on one at a time, or give each its own
server.
