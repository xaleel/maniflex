# 8. Events & Background Jobs

Part 7 added a custom action for placing orders. This part defers the
post-order work — sending a receipt email — to a background job so that a
slow or failing mailer never blocks the purchase response or rolls back the
order transaction.

## Why a background worker

Running side-effects inside the request transaction creates two problems:

- If the email fails, the whole order rolls back — a transient mail outage
  breaks the purchase flow entirely.
- If the order rolls back after a successful email, the email can't be unsent.

Decoupling fixes both: the transaction writes a small job description; a worker
outside the transaction carries it out. If the worker crashes mid-task the job
is retried automatically.

## Wiring up the job queue

The `jobs/` package family provides a durable queue with retries and REST-based
status polling. For the bookstore we use `jobs/sql`, which enqueues inside the
same database transaction as the business write.

Install the queue alongside the server setup in `main.go`:

```go
import (
    "github.com/xaleel/maniflex"
    jobsmaniflex "github.com/xaleel/maniflex/jobs/maniflex"
    "github.com/xaleel/maniflex/jobs"
    jobssql "github.com/xaleel/maniflex/jobs/sql"
)

server := maniflex.New(maniflex.Config{ /* ... */ })
server.MustRegister(Order{}, User{} /* ... */)

db, _ := sqlite.Open("./app.db", server.Registry())
server.SetDB(db)

// jobs/sql takes a database/sql handle, not the maniflex adapter; point it at the
// same database file so jobs live alongside your data. (The "sqlite" driver is
// registered by importing the db/sqlite package above.)
jobsDB, _ := sql.Open("sqlite", "./app.db")
queue := jobssql.New(jobsDB)
jobssql.Migrate(ctx, jobsDB, "sqlite") // "postgres" on PG

// Mount registers the StatusModel and returns a wrapped queue + sink.
// After this, GET /api/job_statuses/:id is available automatically.
sink, queue, err := jobsmaniflex.Mount(server, queue)
if err != nil { log.Fatal(err) }

// Wire up the worker.
w, _ := jobs.NewWorker(jobs.WorkerConfig{
    Source:  queue.(jobs.Source),
    Status:  sink,
    Handlers: map[string]jobs.Handler{
        "send_receipt": sendReceiptHandler(mailer),
    },
})

go w.Run(ctx)
log.Fatal(server.Start())
```

## Enqueueing from the order action

Modify the order placement action from Part 7 to enqueue a job instead of
calling the mailer directly:

```go
server.Action(maniflex.ActionConfig{
    Method: "POST",
    Path:   "/orders",
    Handler: func(ctx *maniflex.ServerContext) error {
        // ... validate, insert order, etc. ...

        jobID, err := queue.Enqueue(ctx.Ctx, jobs.Job{
            Type:     "send_receipt",
            ActorID:  ctx.Auth.UserID,
            Payload:  mustJSON(map[string]any{"order_id": orderID}),
        })
        if err != nil {
            return err
        }

        ctx.Response = &maniflex.APIResponse{
            StatusCode: http.StatusAccepted,
            Data: map[string]any{
                "order_id": orderID,
                "job_id":   jobID,     // clients can poll /api/job_statuses/:job_id
            },
        }
        return nil
    },
})
```

The wrapped `queue` creates an `enqueued` status row before returning, so the
client can poll immediately — no race between enqueue and the first GET.

## The handler

```go
func sendReceiptHandler(mailer Mailer) jobs.Handler {
    return func(ctx context.Context, j jobs.Job) (jobs.Result, error) {
        var p struct {
            OrderID string `json:"order_id"`
        }
        if err := json.Unmarshal(j.Payload, &p); err != nil {
            return jobs.Result{}, err
        }
        return jobs.Result{}, mailer.SendReceipt(ctx, p.OrderID)
    }
}
```

Handlers return `(jobs.Result, error)`. On error the worker retries with
exponential backoff (default up to 3 attempts). After all retries the job is
marked `dead` and the status row records the final error message.

## Polling for completion

The client receives `job_id` in the response and polls until done:

```
POST /api/orders
← 202 {"data": {"order_id": "xyz", "job_id": "01JABC..."}}

GET /api/job_statuses/01JABC...
← 200 {"data": {"status": "enqueued", ...}}

GET /api/job_statuses/01JABC...       (retry after a tick)
← 200 {"data": {"status": "succeeded", "completed_at": "2025-01-15T09:01:02Z"}}
```

No extra endpoint or custom table — the `StatusModel` is wired up automatically
by `Mount`.

## Emitting events from the pipeline

For lighter-weight fan-out — "notify other services every time an `Order` is
created" — the `events.Emit` middleware is a simpler fit than the job queue:

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

`Emit` publishes `order.created` (and `order.updated`, `order.deleted`) to the
bus on the DB-After step — only when the write succeeded. Subscribers in the
same or other processes consume events independently. For WebSocket fan-out to
connected clients, wire a `realtime.Hub` to the bus (see
[Realtime / WebSockets](../advanced-topics/realtime.md)).

## Webhooks

`events.Webhook` delivers events to external URLs with an HMAC signature —
useful for one-off partner integrations. Unlike `events.Emit`, it is an event-bus
*subscriber*, not pipeline middleware, so wire it with `bus.Subscribe`:

```go
bus.Subscribe(ctx, events.Subscription{
    Patterns: []string{"order.*"},
    Handler: events.Webhook(events.WebhookConfig{
        URL:    "https://partner.example.com/orders",
        Secret: os.Getenv("WEBHOOK_SECRET"),
    }),
})
```

## What we built

| Capability | How |
|---|---|
| Decoupled post-order email | `jobs.Queue` — enqueue in action, process in worker |
| Status polling | `jobs/maniflex.Mount` → `GET /api/job_statuses/:id` |
| Automatic retries | `jobs.Job.MaxRetry` + exponential backoff |
| Transactional enqueue | `jobs/sql` inserts the job row in the same DB transaction |
| Domain event fan-out | `events.Emit` on DB-After → event bus subscribers |
| External webhook delivery | `events.Webhook` as a bus subscriber |

## Next

In **[Part 9 — Testing the API](9-testing.md)** we test the whole app end to
end, including the job worker and the polling flow.
