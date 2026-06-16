# 9. Testing the API

A useful test suite for a maniflex app exercises the HTTP layer, not just the
database. The framework is built on `net/http`, so the standard
`httptest.Server` is enough — start an in-memory SQLite, register everything,
hit the routes, assert on responses.

## A test harness

`tests/setup.go`:

```go
package tests

import (
    "log"
    "net/http/httptest"

    "github.com/xaleel/maniflex"
    "github.com/xaleel/maniflex/db/sqlite"

    "bookstore/middleware"
    "bookstore/models"
)

// newTestServer returns a running httptest.Server backed by an in-memory
// SQLite. Each test gets a fresh database.
func newTestServer(t *testing.T) (*httptest.Server, *maniflex.Server) {
    t.Helper()

    server := maniflex.New(maniflex.Config{
        Port:        0,
        PathPrefix:  "/api",
        AutoMigrate: true,
    })

    server.MustRegister(
        models.User{}, models.Author{}, models.Genre{},
        models.Book{}, models.BookGenre{}, models.Review{},
        models.Order{}, models.OrderLine{}, models.OutboxEvent{},
    )

    db, err := sqlite.Open(":memory:", server.Registry())
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { db.Close() })
    server.SetDB(db)

    middleware.Register(server)

    ts := httptest.NewServer(server.Handler())
    t.Cleanup(ts.Close)
    return ts, server
}
```

Two notes:

- **`:memory:`** opens an in-memory SQLite database. There is no file to
  clean up; closing the connection discards it.
- **`server.Handler()`** returns the chi router — `httptest.NewServer`
  wraps it and serves requests in-process.

A small JSON helper keeps tests readable:

```go
func do(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
    t.Helper()
    var rdr io.Reader
    if body != nil {
        b, _ := json.Marshal(body)
        rdr = bytes.NewReader(b)
    }
    req, _ := http.NewRequest(method, ts.URL+path, rdr)
    if token != "" {
        req.Header.Set("Authorization", "Bearer "+token)
    }
    req.Header.Set("Content-Type", "application/json")
    resp, err := ts.Client().Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()
    var out map[string]any
    json.NewDecoder(resp.Body).Decode(&out)
    return resp.StatusCode, out
}
```

## Happy-path sign-up + login

```go
func TestSignupAndLogin(t *testing.T) {
    ts, _ := newTestServer(t)

    code, body := do(t, ts, "POST", "/api/users", "", map[string]any{
        "email":    "alice@example.com",
        "password": "hunter22!",
        "name":     "Alice",
    })
    if code != 201 {
        t.Fatalf("signup: %d %v", code, body)
    }

    code, body = do(t, ts, "POST", "/api/auth/login", "", map[string]any{
        "email":    "alice@example.com",
        "password": "hunter22!",
    })
    if code != 200 || body["data"].(map[string]any)["token"] == "" {
        t.Fatalf("login: %d %v", code, body)
    }
}
```

A new in-memory database for each `t.Run` keeps the tests isolated; nothing
to truncate, nothing to seed beyond the test's own writes.

## Validation failures

The exact response shape matters because clients depend on it. Pin it down:

```go
func TestInvalidEmail(t *testing.T) {
    ts, _ := newTestServer(t)
    code, body := do(t, ts, "POST", "/api/users", "", map[string]any{
        "password": "hunter22!",
        "name":     "Alice",
        // email missing
    })
    if code != 422 {
        t.Fatalf("got %d, want 422", code)
    }
    if body["error"].(map[string]any)["code"] != "VALIDATION_FAILED" {
        t.Fatalf("code = %v", body["error"])
    }
}
```

## Stock contention

The order-placement transaction is the most interesting code path. The
test starts two goroutines that race for the last unit:

```go
func TestStockContention(t *testing.T) {
    ts, _ := newTestServer(t)
    tok, bookID := seedOneBookOneCustomer(t, ts, 1) // stock = 1

    var wg sync.WaitGroup
    results := make([]int, 2)

    for i := range results {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            results[i], _ = do(t, ts, "POST", "/api/orders/place", tok, map[string]any{
                "lines": []map[string]any{{"book_id": bookID, "quantity": 1}},
            })
        }(i)
    }
    wg.Wait()

    // Exactly one 201 Created, exactly one 409 Conflict.
    sort.Ints(results)
    if results[0] != 201 || results[1] != 409 {
        t.Fatalf("expected one 201 and one 409, got %v", results)
    }
}
```

`LockForUpdate` ensures only one of the two transactions wins; the other
sees the decremented stock and aborts with `OUT_OF_STOCK`.

## Worker tests

The background worker is plain Go. Inject a stub mailer and assert on it:

```go
func TestOutboxWorker(t *testing.T) {
    ts, server := newTestServer(t)
    tok, bookID := seedOneBookOneCustomer(t, ts, 5)

    // Place an order — should write an outbox row.
    do(t, ts, "POST", "/api/orders/place", tok, map[string]any{
        "lines": []map[string]any{{"book_id": bookID, "quantity": 1}},
    })

    stub := &captureMailer{}
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go jobs.RunOutboxOnce(ctx, server, stub) // single iteration

    // Give the worker a moment.
    if !waitFor(time.Second, func() bool { return len(stub.Sent) == 1 }) {
        t.Fatalf("expected 1 email, got %d", len(stub.Sent))
    }
    if stub.Sent[0].Subject != "Thank you for your order" {
        t.Fatalf("wrong subject: %q", stub.Sent[0].Subject)
    }
}
```

Run worker logic in a one-shot variant (`RunOutboxOnce`) for testability —
or accept a `context.Context` and cancel it after the assertions pass. Both
patterns avoid `time.Sleep` in tests.

## The framework's own test suite

The framework ships an end-to-end suite under `tests/e2e/`. It is the
canonical reference for what a thorough maniflex test looks like — every step
of the pipeline, every adapter, every middleware option. Run it with:

```bash
go test ./tests/e2e/...
```

…and look at the test files for patterns you can lift into your own suite.

## Coverage strategy

For a typical bookstore-shaped app, a useful test split:

- **Per model**: happy-path create + read + update + delete.
- **Per `mfx:` tag rule**: at least one negative test (`required`, `enum`,
  `min`/`max`, `unique`).
- **Per custom middleware**: at least one happy-path and one rejection.
- **Per action**: happy path, a representative failure, and a contention
  test.
- **The outbox worker**: receives an event, processes it, marks it done.

That covers the surface area without exploding into combinatorial tests of
every filter operator and every relation include — those are exercised by
the framework's own e2e suite, which you depend on transitively.

## Next

In **[Part 10 — Deploying to Production](10-deploy.md)** we swap SQLite for
PostgreSQL, drive configuration from environment variables, enable the
health probe, and produce a single binary suitable for a container image.
