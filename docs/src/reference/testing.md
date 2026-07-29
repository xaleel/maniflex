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

## Databases

- `SQLite()` is the default and creates a distinct in-memory database.
- `SQLite(path)` uses a file-backed database when persistence behaviour matters.
- `Postgres(dsn)` creates and later drops an isolated random schema.
- A custom `DatabaseFactory` can return any `maniflex.DBAdapter` plus an
  optional cleanup callback.

The harness owns adapters returned by its database factory and closes them
after cleanup. Do not share one adapter between concurrently running harness
servers.
