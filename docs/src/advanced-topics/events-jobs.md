# Events & Background Jobs

maniflex offers two complementary mechanisms for work that happens outside the
request pipeline: an **event bus** for lightweight domain-event fan-out, and a
**job queue** for durable, retriable background work.

| Mechanism | When to use |
|---|---|
| Event bus (`events/*`) | Notify other services or modules that something happened. Fire-and-forget. |
| Job queue (`jobs/*`) | Do something reliably after a request — report generation, email, reconciliation. Needs retry and status tracking. |

---

## Event bus

The event bus lets pipeline middleware publish domain events that any number of
subscribers consume independently. An `events.Emit` call on the DB-After step
publishes `user.created`, `order.placed`, etc. to whichever bus is wired up:

```go
import (
    "github.com/xaleel/maniflex/events"
    "github.com/xaleel/maniflex/events/redis"
)

bus := redis.New(redisClient, "myapp") // prefix namespaces the Redis stream keys
server.Pipeline.DB.Register(
    events.Emit(bus),
    maniflex.ForModel("Order"),
    maniflex.AtPosition(maniflex.After),
)
```

Subscribers register a `Subscription`:

```go
bus.Subscribe(ctx, events.Subscription{
    Patterns: []string{"order.*"},
    Handler:  func(ctx context.Context, e events.Event) error { /* ... */ return nil },
})
```

For WebSocket fan-out, connect a `realtime.Hub` to the bus — see
[Realtime / WebSockets](realtime.md).

### What the payload carries

`Event.Data` is the written row, keyed by **database column name** — not by
`json` name, and not the response shape. It is deliberately not the response
projection: locale resolution and `ctx.RedactResponseField` masking are
decisions made for one requesting caller, and an event is durable, replayable,
and read by subscribers who never made that request.

Four kinds of column are stripped before the event leaves:

| Excluded | Why |
|---|---|
| `mfx:"hidden"` | never leaves the server |
| `mfx:"writeonly"` | never read back — password hashes and the like |
| `mfx:"encrypted"` | the row reaching `Emit` is already decrypted, so emitting it would publish the plaintext |
| `{field}_hmac` | the searchable digest companion of an encrypted+unique column |

This matters because the payload is not a transient in-memory value: it is
persisted verbatim to `event_outbox.payload`, replayed by the outbox relayer,
pushed to every WebSocket and SSE client through the hub, and written to
whatever the broker retains. An encrypted column emitted in the clear defeats
the at-rest guarantee everywhere downstream at once.

`maniflex.RedactRecord(model, row)` applies the same exclusion set, if you build
an event by hand (see the custom-action note below) or serialize `ctx.DBResult`
in your own middleware.

### Dead-lettering

Set `Subscription.DLQ` (or `RelayOptions.DLQType` on the outbox relayer) to
re-publish an event that exhausted its attempts under a separate type, through
the same broker. Both paths produce the same payload:

| | |
|---|---|
| `ID` | **a fresh one** — the original was already published under its ID, so reusing it gets the dead-letter dropped by any downstream deduper |
| `Type` | the configured DLQ type |
| `Headers` | every original header, plus `original_type` and `original_id` |

Everything else is copied unchanged, so the dead-letter carries the same `Data`,
`Model`, `RecordID` and `TenantID` as the event it came from.

A DLQ publish that itself fails is logged and the event is then lost — the DLQ
rides the same broker that was already failing, so a persistent outage takes the
dead-letters with it.

> **Custom actions emit manually.** `events.Emit` runs on the DB step, which
> [custom actions](actions.md) skip — so a `server.Action` handler never fires
> the middleware and must publish to the bus itself:
>
> ```go
> data, _ := json.Marshal(maniflex.RedactRecord(ctx.Model, ctx.DBResult))
> err := bus.Publish(ctx.Ctx, events.Event{
>     Type:     "order.cancelled",
>     Model:    "Order",
>     RecordID: orderID,
>     Data:     data,
> })
> ```
>
> Nothing redacts a hand-built payload for you — marshal `ctx.DBResult` directly
> and you publish the plaintext of every encrypted column.
>
> For the transactional outbox, publish inside the action's own transaction so
> the event commits atomically with the write.

Available adapters: `events/redis`, `events/kafka`, `events/nats`,
`events/rabbitmq`. The in-process adapter (`inproc.New()` from
`github.com/xaleel/maniflex/events/inproc`) ships in the core module for tests.

> **Broker adapters are nested modules.** Adapters with heavy dependencies (e.g.
> NATS) ship as their own Go modules — `go get github.com/xaleel/maniflex/events/nats`
> — so the core module stays dependency-light. Pin each one explicitly in `go.mod`.

> **NATS: one bus binds one JetStream stream.** `nats.New(nc, stream)` ties a bus
> to a single stream for both publish and subscribe, and JetStream rejects two
> streams whose subjects overlap. A service that publishes its own subjects while
> consuming another service's subjects needs a stream-ownership decision — either
> a shared stream, or consume from the owning service's stream. Create the stream
> yourself (scoped to your business subjects, not `">"`); `New` does not create it.

---

## Job queue

The `jobs/` packages provide a producer/consumer queue with retries, dead-letter
routing, and optional status persistence through the REST layer.

### Adapters

| Package | Backing store | Transactional enqueue | Best for |
|---|---|---|---|
| `jobs/inproc` | goroutine pool | no (best-effort) | tests, single-binary dev |
| `jobs/sql` | Postgres or SQLite | **yes** — enqueue in the same `ctx.Tx` | production (recommended) |
| `jobs/redis` | Redis Streams / BRPOP | no | high-throughput fleets |

All three share the same `jobs.Queue` and `jobs.Source` interfaces so swapping
adapters is a one-line change.

### Defining and enqueueing a job

```go
import (
    "database/sql"

    "github.com/xaleel/maniflex/jobs"
    jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// jobs/sql takes a database/sql handle, not the maniflex DB adapter.
db, _ := sql.Open("sqlite", "./app.db")
queue := jobssql.New(db)
if err := jobssql.Migrate(ctx, db, "sqlite"); err != nil { /* ... */ } // "postgres" on PG
```

### Lanes and secrets

- **Separate lanes:** run an isolated queue on its own table so a type-restricted
  worker can't interfere with other jobs. Pass `WithTableName` to **both** `New`
  and `Migrate` (indexes are renamed to match, so two queues share one DB):

  ```go
  otp := jobssql.New(db, jobssql.WithTableName("otp_jobs"))
  jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("otp_jobs"))
  ```

- **Encrypt payloads at rest:** payloads are stored as cleartext JSON by default.
  Pass `WithPayloadCipher(cipher)` (any `Encrypt([]byte)`/`Decrypt([]byte)`
  implementation) to encrypt the payload column; stored values are prefixed
  `encq:` and decrypted transparently on dequeue.

- **Unhandled types are requeued, not killed:** a worker that lacks a handler for
  a job's type now requeues it (so another worker can claim it) instead of
  dead-lettering it — safe for a type-restricted worker sharing a table.

```go

// Inside a pipeline middleware or action handler:
id, err := queue.Enqueue(ctx, jobs.Job{
    Type:     "send_receipt",
    ActorID:  ctx.Auth.UserID,
    TenantID: ctx.Auth.TenantID,
    Payload:  json.RawMessage(`{"order_id":"abc"}`),
})
```

Fields worth knowing:

| Field | Effect |
|---|---|
| `Type` | Selects the handler on the Worker (required) |
| `MaxRetry` | Max attempts before dead. Default 3. |
| `NotBefore` | Delay execution until this time (use `EnqueueAt` as a shortcut) |
| `GroupKey` | At most one job with this key runs at a time — useful for per-tenant serialisation |
| `TraceID` | Propagated to the handler context for end-to-end trace correlation |

### The Worker

```go
import "github.com/xaleel/maniflex/jobs"

w, err := jobs.NewWorker(jobs.WorkerConfig{
    Source:   queue.(jobs.Source),
    Handlers: map[string]jobs.Handler{
        "send_receipt": func(ctx context.Context, j jobs.Job) (jobs.Result, error) {
            var p struct{ OrderID string `json:"order_id"` }
            json.Unmarshal(j.Payload, &p)
            return jobs.Result{}, mailer.SendReceipt(ctx, p.OrderID)
        },
    },
    Concurrency: 8,        // goroutines; default = GOMAXPROCS
    Logger:      slog.Default(),
})

ctx, cancel := context.WithCancel(context.Background())
go w.Run(ctx)

// On shutdown:
cancel()
w.Shutdown(shutdownCtx)
```

> **Migrate before you launch background goroutines.** `server.Go(fn)` (and a bare
> `go w.Run(ctx)`) starts running immediately, but `AutoMigrate` only runs inside
> `Start()`. A worker that touches a table before `Start()` migrates it races table
> creation. When you launch workers yourself, call `server.MigrateOnly(ctx)` after
> `SetDB` and **before** starting them so the tables exist.

`Result` carries an optional `URL` (pre-signed storage URL for file outputs)
and `Output` (small structured JSON). Both are surfaced through the status model
below.

### StatusModel — REST polling

Mount the status model once, alongside other model registrations:

```go
import jobsmaniflex "github.com/xaleel/maniflex/jobs/maniflex"

sink, queue, err := jobsmaniflex.Mount(server, rawQueue)
if err != nil { log.Fatal(err) }

// Pass sink to the worker:
w, _ := jobs.NewWorker(jobs.WorkerConfig{
    Source:  queue.(jobs.Source),
    Status:  sink,
    Handlers: handlers,
})
```

`Mount` registers a `StatusModel` (table `job_statuses`) and returns:

- **`sink`** — a `jobs.StatusSink` to pass to `WorkerConfig.Status`; the worker
  writes a row for every lifecycle transition.
- **`queue`** — a wrapped `jobs.Queue`; every `Enqueue` call creates an initial
  `enqueued` status row so clients can poll immediately.

The REST layer exposes these endpoints automatically (no extra code):

```
GET  /api/job_statuses           list (filterable, paginated)
GET  /api/job_statuses/:id       single row
POST /api/job_statuses           → 405 (worker-only)
```

A typical client flow after an action returns `{"job_id": "abc"}`:

```
GET /api/job_statuses/abc
→ {"data": {"status": "enqueued", ...}}

GET /api/job_statuses/abc   (poll until done)
→ {"data": {"status": "succeeded", "result_url": "https://...", "completed_at": "..."}}
```

Status values: `enqueued → running → succeeded | failed | dead | cancelled`.

#### Scope

By default, unauthenticated requests see all rows. When a caller is
authenticated, the built-in force-filter restricts the list to their own
`actor_id`; callers with the `admin` role see everything. Override the role
name with `MountOptions.AdminRole`.

### Atomic enqueue with `jobs/sql`

When `jobs/sql` is the adapter and a `maniflex.WithTransaction` middleware is
active, `queue.Enqueue` runs its `INSERT` through the same `*sql.Tx`:

```go
// Service step:
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if err := next(); err != nil {  // DB write commits first
        return err
    }
    _, err := queue.Enqueue(ctx.Ctx, jobs.Job{
        Type:    "reconcile_inventory",
        Payload: json.RawMessage(`{"product_id":"` + productID + `"}`),
    })
    return err
}, maniflex.ForModel("Order"), maniflex.AtPosition(maniflex.After))
```

If the transaction rolls back, the job row never appears. If the process crashes
after commit, the job row is durable and the worker will pick it up. This
eliminates the "DB committed but job lost" race that an in-memory queue cannot
prevent.

### GroupKey — serialised execution

Set `GroupKey` to ensure at most one job for a given key runs at a time:

```go
queue.Enqueue(ctx, jobs.Job{
    Type:     "generate_payroll",
    GroupKey: "tenant:" + tenantID,  // one payroll run per tenant at a time
})
```

The `jobs/sql` adapter enforces this via `SELECT … SKIP LOCKED`; `jobs/inproc`
tracks running keys in memory.

### Retry and dead-letter

When a handler returns an error the worker re-queues the job after an
exponential backoff (base 1 s, cap 5 min). After `Job.MaxRetry` attempts the
job is marked `dead` and the status row records the final error. Set
`WorkerConfig.DLQType` to route dead jobs to a separate handler for inspection
or alerting.

### Cancellation

When the inner queue implements `jobs.Cancellable` (both `jobs/inproc` and
`jobs/sql` do), the wrapped queue returned by `Mount` also implements it:

```go
c := queue.(jobs.Cancellable)
c.Cancel(ctx, jobID)   // marks the job cancelled in the queue and updates the status row
```

Only jobs that have not yet started can be cancelled; a running job must finish
or fail before the status row moves.

### Completion events (optional)

Set `WorkerConfig.EventBus` to publish `job.{type}.completed` and
`job.{type}.failed` events on every terminal transition. Pair with a
`realtime.Hub` to push completion notifications to connected clients without
polling:

```go
w, _ := jobs.NewWorker(jobs.WorkerConfig{
    // ...
    EventBus: bus,   // any value implementing Publish(ctx, type, payload) error
})
```

### Scheduled jobs with `jobs/cron`

`jobs/cron` provides a minimal ticker that calls `Queue.EnqueueAt` on a fixed
interval. It does not offer durable cron (if a replica is down at fire time the
tick is missed); for durable scheduling, combine `jobs/sql` with a
`next_fire_at` column in your model. For field-based transitions (auto-publish,
auto-expire), see [Scheduled Fields & Runner](scheduled.md).

Schedules are fixed **intervals** (`Every`), not cron expressions:

```go
import (
    "time"

    "github.com/xaleel/maniflex/jobs"
    "github.com/xaleel/maniflex/jobs/cron"
)

cr := cron.New(queue, nil) // nil logger → slog.Default()
cr.Add(cron.Entry{
    Every: 24 * time.Hour,
    Job:   jobs.Job{Type: "daily_report"},
})
cr.Start(ctx) // returns immediately; cr.Stop() halts the tickers
```
