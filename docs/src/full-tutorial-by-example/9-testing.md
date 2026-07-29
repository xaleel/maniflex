# 9. Testing the API

A useful test suite for a Maniflex app exercises the HTTP layer, not just the
database. The supported `maniflextest` module starts a real HTTP server,
migrates a fresh in-memory SQLite database, and registers all cleanup with the
test:

```bash
go get github.com/xaleel/maniflex/maniflextest
```

## A test server

The harness takes the same models and setup function as the application. Calls
such as `POST("/widgets", ...)` are relative to the configured API prefix
(`/api` by default).

```go
{{#include ../../../maniflextest/consumer_test.go:basic-harness}}
```

`maniflextest.New` completes the repetitive integration-test work:

- registers the supplied models and application setup;
- opens a unique in-memory SQLite database and runs `MigrateOnly`;
- starts an `httptest.Server`;
- shuts down services, drops test data, and closes the adapter automatically.

Each call to `New` is isolated, including separate calls in nested `t.Run`
tests. Use `server.App()` when a worker or background operation needs the
underlying `*maniflex.Server`.

## Test principals

`maniflextest.As` injects a complete `maniflex.AuthInfo` before the
application's Auth middleware runs:

```go
admin := maniflextest.Human("user-42", "admin")
response := server.POST(
    "/orders",
    map[string]any{"book_id": bookID},
    maniflextest.As(admin),
)
response.AssertStatus(http.StatusCreated)
```

Use `ServiceAccount(id, scopes...)` for machine callers. For tenant, claim, or
session-specific tests, construct `maniflex.AuthInfo` directly and pass it to
`As`.

The injected principal is only a test transport; it is not production
authentication. Set `DisableTestAuth: true` and use `maniflextest.Bearer` when
testing a real JWT or API-key middleware end to end.

## Typed responses

Responses are buffered so status, headers, raw bytes, envelopes, and typed
models can all be asserted:

```go
created := server.POST("/books", input).AssertStatus(http.StatusCreated)
book := maniflextest.DecodeData[models.Book](created)

books := maniflextest.DecodeDataList[models.Book](
    server.GET("/books?sort=title").AssertStatus(http.StatusOK),
)
```

For error paths, use `response.ErrorCode()`:

```go
response := server.POST("/users", map[string]any{"name": "Alice"})
response.AssertStatus(http.StatusUnprocessableEntity)
if code := response.ErrorCode(); code != "VALIDATION_ERROR" {
    t.Fatalf("error code: got %q", code)
}
```

## Fixtures and factories

Fixtures are ordinary API creates, so they pass through validation, middleware,
and hooks instead of inserting a database shape the application could never
produce:

```go
fixtures := server.Seed(
    maniflextest.Fixture{
        Name: "alice",
        Path: "/users",
        Body: map[string]any{"name": "Alice", "email": "alice@example.com"},
    },
)
aliceID := fixtures.ID(t, "alice")
```

Use a factory for repeated data:

```go
books := maniflextest.Factory(
    "book",
    "/books",
    10,
    func(i int) map[string]any {
        return map[string]any{"title": fmt.Sprintf("Book %02d", i)}
    },
    maniflextest.As(maniflextest.Human("author-1")),
)
fixtures := server.Seed(books...)
firstID := fixtures.ID(t, "book[0]")
```

Names are stable (`book[0]`, `book[1]`, and so on), making relationships easy
to express without relying on insertion order or hard-coded IDs.

## PostgreSQL-specific tests

Most application behaviour should use the fast SQLite default. When SQL
dialect, locking, or transaction semantics matter, give the harness a
PostgreSQL DSN:

```go
server := maniflextest.New(t, maniflextest.Options{
    Models:   appModels(),
    Setup:    registerApplication,
    Database: maniflextest.Postgres(os.Getenv("MANIFLEX_TEST_PG_DSN")),
})
```

Every server receives a randomly named schema. Cleanup drops the entire schema,
so tests do not share rows and there is no truncation list to maintain. The
database user must be allowed to create and drop schemas.

## Contention and workers

For a contention test, create one harness server and send concurrent requests
through it. Do not call `t.Fatal` from request goroutines; collect each
response's `StatusCode` and assert from the test goroutine after they join.

Set `StartServices: true` when the behaviour under test needs registered
services or lifecycle hooks. Prefer a one-shot worker entry point where
possible; otherwise make the worker accept a `context.Context` and stop it
before the test returns. Harness cleanup calls `Server.Shutdown`, so framework
background work is drained rather than abandoned.

See [Testing Applications](../reference/testing.md) for the complete supported
surface and customization points.

## Coverage strategy

For a typical bookstore-shaped app, a useful split is:

- per model: happy-path create, read, update, and delete;
- per `mfx:` rule: at least one representative rejection;
- per custom middleware: one accepted and one rejected request;
- per action: a happy path, a representative failure, and any important
  contention case;
- per worker: one event is received, processed, and acknowledged.

The framework's own end-to-end suite covers adapter and query permutations.
Application tests should concentrate on the policies and business behaviour
the application adds.

## Next

In **[Part 10 — Deploying to Production](10-deploy.md)** we swap SQLite for
PostgreSQL, drive configuration from environment variables, enable the health
probe, and produce a single binary suitable for a container image.
