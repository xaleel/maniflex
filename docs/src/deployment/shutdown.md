# Graceful Shutdown

`server.Start()` blocks on the HTTP listener and additionally listens for
`SIGINT` and `SIGTERM`. When either signal arrives, the server stops accepting
new connections and gives in-flight requests up to
**`Config.ShutdownTimeout`** to finish before forcing the listener closed.

## How it works

1. A signal arrives.
2. `http.Server.Shutdown(ctx)` is called with a deadline of
   `Config.ShutdownTimeout` (default: 30 seconds).
3. The listener stops accepting new connections immediately.
4. In-flight requests are allowed to complete — including their pipeline
   middleware, transaction commits, and Response writes.
5. Background work spawned by the request — audit writes, cache invalidations,
   event publishes — is waited for on the same budget.
6. When all requests have finished, or the deadline elapses, the database
   adapter's `Close()` is called.
7. `Start()` returns.

If the deadline passes with requests still running, the underlying TCP
connections are closed — those requests fail mid-flight but the process exits
cleanly.

## Embedding `Handler()` in your own server

An embedding owns the HTTP listener, while Maniflex still owns its registered
services, `Server.Go` loops, and request background work. Start and stop those
two halves explicitly. Finish model, action, and middleware registration before
calling `MigrateOnly`, which validates and seals that configuration:

```go
if err := server.MigrateOnly(ctx); err != nil {
    log.Fatal(err)
}
if err := server.StartServices(); err != nil {
    log.Fatal(err)
}

httpServer := &http.Server{
    Addr:    ":8080",
    Handler: server.Handler(),
}
go httpServer.ListenAndServe()

// On termination, stop requests first so none can add new background work.
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = httpServer.Shutdown(shutdownCtx)
_ = server.Shutdown(shutdownCtx)
```

`StartServices` validates and seals the router, runs `OnStart`, and starts
services in registration order, but does not migrate or open a listener.
`Server.Shutdown` then stops those services in reverse order, runs
`OnShutdown`, cancels `Server.Go`, and drains both application and request
background work.

An embedding with no services or lifecycle hooks may omit `StartServices`.
It must still call `Server.Shutdown` after its own `http.Server.Shutdown`;
Maniflex will cancel and drain `Server.Go` and `ctx.GoBackground` work without
running lifecycle phases that never started.

## Event buses

A bus is not owned by the server, so nothing closes it for you. `inproc.Bus.Close`
stops accepting events and waits for in-flight handlers, bounded by
`Options.DrainTimeout`; a non-nil error means the drain did not finish and those
events were not processed. Broker adapters close their connections.

## Tuning `ShutdownTimeout`

Pick the value based on the longest legitimate request your service serves:

| Environment | Suggested `ShutdownTimeout` |
|---|---|
| Tests | `0–1s` — exit instantly |
| Lambdas / fast-cycling containers | `5–10s` |
| General OLTP API | `30s` (default) |
| Bulk import or large file uploads | `60s+` |

Setting `ShutdownTimeout` shorter than your slowest request will sever it on
shutdown. Setting it longer makes deploys slower with no benefit beyond the
slowest real request.

## Why graceful shutdown matters

Cutting a request mid-write produces inconsistent state at the boundary — a
write that may or may not have committed, a webhook that may have fired but
not been recorded, a client that may or may not have seen the response. The
graceful path:

- ensures transactions commit or roll back cleanly,
- lets the Response step write its envelope before the connection drops,
- gives `maniflex.WithTransaction`'s deferred rollback a chance to run.

For Kubernetes deployments, set `terminationGracePeriodSeconds` on the pod to
a value larger than `ShutdownTimeout`, otherwise the orchestrator will send
`SIGKILL` before the graceful handler completes.

## Manual shutdown

For tests or custom lifecycle code, the same graceful path is available
without waiting for a signal:

```go
go server.Start()
// ... run tests ...
server.Shutdown(ctx)
```

`Shutdown` uses the supplied context as the deadline. Pass `context.Background`
for "wait as long as it takes"; pass a `context.WithTimeout` for an explicit
budget.

It is safe to call from any goroutine at any point in the server's life — the
snippet above races `Start` by construction, and the outcome does not depend on
who wins. A `Shutdown` that lands while the server is still booting (migrating,
or waiting on a service that dials its backend) **countermands** the boot: the
listener is never opened, `Start` unwinds whatever it had already brought up and
returns `nil`, and `Shutdown` waits for that to finish before returning. On a
server that never started services or a listener, it still cancels and drains
any `Server.Go` and request background work before returning.

`Shutdown` is terminal rather than a pause: a server that has been shut down will
not open a listener afterwards. `Start` following `Shutdown` returns
`maniflex.ErrStopped`. A `Server` is not restartable — build a new one.

Only one caller may own startup. A second `Start`, `StartWithContext`, or
`StartServices` while a startup mode is active returns
`maniflex.ErrAlreadyStarted`; it does not repeat validation, migration, service
startup, or listener publication. After shutdown or a startup failure, later
startup calls return `maniflex.ErrStopped`. Both errors support `errors.Is`.

## Background writes

Audit-log writes, cache invalidations (`db.Invalidate`), and async file
cleanups (`Config.FilesConfig.Storage` with `mfx:"auto_delete"` fields) run on
goroutines tracked by the server. `Shutdown` waits for those to drain
within the same deadline as the HTTP listener. If the deadline elapses
with goroutines still in flight, the server logs a warning with the
in-flight count and proceeds — the goroutines see their context cancelled
and exit on the next checkpoint.

Custom middleware can opt into the same lifecycle via
`ctx.GoBackground(fn func(context.Context))`; the supplied context is
independent of the request (which has already returned) but IS cancelled
when shutdown's deadline hits.

### Panics on background goroutines

A panic in a `ctx.GoBackground` task or a `server.Go` loop is recovered, logged
through `Config.PanicLogger` with its stack, and contained — it does not take
the process down. Without that, a panic in one audit write killed a server that
was otherwise healthy, which is worse than the request panic the framework
already recovers.

Containment has a cost worth naming: a `server.Go` loop that panics is **gone
until the next restart** while the process keeps serving HTTP. The ERROR record
is the only signal, so treat it as one. `Config.OnBackgroundPanic` is the hook
for acting on it programmatically:

```go
cfg.OnBackgroundPanic = func(recovered any, stack []byte) {
    metrics.Inc("background_panic")
    os.Exit(1) // let the orchestrator restart a half-dead process
}
```

It runs on the panicking goroutine after the log is written, so it must not
block, and a panic inside the hook is not recovered again. Leave it `nil` and
the panic is only logged.

## Supervised services & lifecycle hooks

Applications often own long-lived background components — a poller, cache
warmer, queue consumer, or an in-memory pool manager — that must start *after*
the database is ready and stop *cleanly* before the process exits. Register
them as services and the framework folds them into the boot and shutdown
lifecycle instead of you hand-supervising them around `Start`.

```go
type Service interface {
    Start(ctx context.Context) error // ctx is cancelled at shutdown
    Stop(ctx context.Context) error  // carries what's left of the shutdown budget
}

server.AddService(pool)                          // a custom Service
server.AddService(maniflex.ServiceFunc(startFn)) // adapter for a bare start func
```

`AddService` must be called before `Start` or `StartServices` — it panics once
either startup path has been entered, including while earlier services are
still starting. Startup fixes the service list when it begins, so a service
registered after that point would never be started, and a panic is a better
answer than a component that silently never runs. The same applies to `Action`,
`RealtimeDoc` and `EnableGlobalSearch`, which fix the routing table.

For app-scoped fire-and-forget work (e.g. a periodic reconciler) that doesn't
need an ordered `Stop`, use `server.Go`. Its context is cancelled when shutdown
begins, and the goroutine is drained before `Start` returns:

```go
server.Go(func(ctx context.Context) {
    t := time.NewTicker(time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            reconcile(ctx)
        }
    }
})
```

Callers that want a hook without defining a `Service` type can set the
lightweight `Config.OnStart` / `Config.OnShutdown` functions.

**Boot order:** `migrate → OnStart → Service.Start (registration order) →
listen`. The embedded sequence performs migration explicitly, then
`StartServices` runs `OnStart → Service.Start`; the caller opens its listener.
A `StartServices`, `Start`, or `OnStart` error aborts startup; services that
already started are stopped in reverse first.

**A failed boot still tears down.** Whatever boot managed to bring up is put back
down before `Start` returns the error — including the `server.Go` loops, which
run from the moment you call `server.Go`, not from `Start`. If the listener fails
to bind (the port is taken), the services stop and the goroutines drain exactly as
they do on the graceful path, in a `ShutdownTimeout` window of its own; if the
migration or a service refused to start, the loops are cancelled and awaited. Your
goroutines are never abandoned mid-write, however boot ends.

**Shutdown order:** `http.Shutdown → Service.Stop (reverse order) → OnShutdown
→ drain server.Go + ctx.GoBackground goroutines`. The `Start` context is
cancelled when shutdown begins so loops wind down on their own.

**One budget, shared.** Every phase runs on the *same* deadline context —
`ShutdownTimeout`, or whatever deadline you pass to `server.Shutdown(ctx)`. The
phases are sequential, so they draw down a single window rather than each getting
a fresh one: a drain that eats 25 of 30 seconds leaves `Stop`, `OnShutdown` and
the goroutine drain 5 seconds between them. Honour the ctx you are handed — that
is what keeps total shutdown inside the window your orchestrator allows before it
escalates to `SIGKILL`.

`AddService`, `OnStart`, and `server.Go` are inert for apps that register
nothing — there is no behavioural change unless you opt in.

## Probes during shutdown

The two probes deliberately diverge the moment shutdown begins:

| Endpoint | During the drain |
|---|---|
| `GET {prefix}/ready` | `503 {"status":"stopping"}` immediately, without waiting on any dependency check — this is what deregisters the pod from its load balancer |
| `GET {prefix}/live` | still `200`, for as long as the process can answer |

Liveness must stay green here. A liveness probe that fails during the drain
earns the process a `SIGKILL` in the middle of the requests the drain exists to
finish — the exact outcome graceful shutdown is meant to prevent.

Requests already in flight are honoured throughout, so the endpoints keep
answering until the listener closes.
