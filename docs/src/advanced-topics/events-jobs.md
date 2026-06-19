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
subscribers consume independently. A `service.Emit` call on the DB-After step
publishes `user.created`, `order.placed`, etc. to whichever bus is wired up:

```go
import (
    "github.com/xaleel/maniflex/events/redis"
    "github.com/xaleel/maniflex/middleware/service"
)

bus := redis.New(redisClient)
server.Pipeline.DB.Register(
    service.Emit(bus),
    maniflex.ForModel("Order"),
    maniflex.AtPosition(maniflex.After),
)
```

Subscribers call `bus.Subscribe(ctx, "order.*", handler)`. For WebSocket
fan-out, connect a `realtime.Hub` to the bus — see
[Realtime / WebSockets](realtime.md).

Available adapters: `events/redis`, `events/kafka`, `events/nats`,
`events/rabbitmq`. The in-process adapter (`events.NewInProcessBus`) ships in
the core module for tests.

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
    "github.com/xaleel/maniflex/jobs"
    jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// During startup, after opening the DB:
queue := jobssql.New(db)
if err := jobssql.Migrate(ctx, db); err != nil { /* ... */ }

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

```go
import "github.com/xaleel/maniflex/jobs/cron"

cr := cron.New(queue, cron.Config{
    Jobs: []cron.Entry{
        {Type: "daily_report", Schedule: "@daily"},
    },
})
go cr.Run(ctx)
```
