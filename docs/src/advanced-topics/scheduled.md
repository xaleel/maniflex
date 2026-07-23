# Scheduled Fields & the Runner

A `mfx:"scheduled"` tag on a `*time.Time` field declares a time-driven
transition: when the timestamp falls into the past, the framework applies
a configured action to the row. The mechanism is small but covers a
surprising number of real workflows — auto-publish, auto-archive,
soft-delete after expiry, scheduled status transitions.

This page covers both halves: the tag (declarative, per-model) and the
runner (the background goroutine that actually applies transitions).

## The tag

`mfx:"scheduled"` must appear on a `*time.Time` field (the pointer type
is required so "unset" is distinguishable from the zero time). The tag
takes one action and any number of qualifiers, separated by semicolons:

```go
type Post struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt

    Title  string `json:"title"`
    Status string `json:"status" mfx:"required,enum:draft|published|archived,default:draft"`

    // Auto-publish: set status=published when publish_at falls in the past.
    PublishAt *time.Time `json:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`

    // Auto-archive: set status=archived once archive_at falls in the past
    // (no from= — applies regardless of current status).
    ArchiveAt *time.Time `json:"archive_at" mfx:"scheduled;field=status;to=archived"`

    // Auto-soft-delete: requires WithDeletedAt above.
    ExpiresAt *time.Time `json:"expires_at" mfx:"scheduled;soft-delete"`
}
```

### Actions

Exactly one action per scheduled field:

| Action | Effect when the timestamp passes |
|---|---|
| `soft-delete` | sets the soft-delete marker — requires `maniflex.WithDeletedAt` or `WithIsDeleted` |
| `hard-delete` | physically deletes the row, regardless of soft-delete config |
| `field=NAME;to=VALUE` | sets the named field to the value |

### Qualifiers

The `field=...;to=...` action accepts optional qualifiers:

| Qualifier | Effect |
|---|---|
| `from=VALUE` | apply only when the named field currently equals this value |
| `to=VALUE` | the value to assign (required for `field=...`) |

`from=` and `to=` are validated against the field's `enum` (if any) at
registration time — a typo aborts the boot, not the first sweep.

### Validation at registration

Every scheduled tag is resolved when `ScanModel` runs. Configurations
that don't make sense are reported and the field is dropped from the
runner's scope:

- Field type must be `*time.Time`.
- Exactly one of `soft-delete`, `hard-delete`, `field=` is required.
- `soft-delete` requires the model to be soft-deletable.
- `field=` requires a `to=` and references an existing column.
- `from=` / `to=` must be members of the target field's `enum`, if it
  has one.

A scheduled column automatically gets an `IndexSpec` added to the model
so the runner can locate due rows without a full scan.

## The runner

The runner lives in `maniflex/scheduled` (its own satellite-style package).
It is opt-in — declaring scheduled tags makes the rows ready to be acted
on, but nothing happens until a runner is started.

```go
import "github.com/xaleel/maniflex/scheduled"

runner, err := scheduled.New(server, scheduled.Config{
    Interval:  time.Minute,
    BatchSize: 500,
})
if err != nil {
    log.Fatal(err)
}

ctx, cancel := context.WithCancel(context.Background())
defer cancel()
runner.Start(ctx)
defer runner.Stop()
```

`scheduled.New` walks the registry, picks up every model that declares a
scheduled field, and binds them to the runner. A registry with no
scheduled fields produces a usable no-op runner — callers can wire it
unconditionally and pay no cost.

### `Config`

| Field | Default | Purpose |
|---|---|---|
| `Interval` | `1m` | how often the loop ticks |
| `BatchSize` | `500` | maximum rows processed per (model, spec) per tick |
| `Logger` | `slog.Default()` | structured log sink |
| `Clock` | `time.Now().UTC` | injectable; tests override |
| `OnDelete` | nil | callback `func(model, id string)` after a delete commits |
| `OnSetField` | nil | callback `func(model, id, field, to string)` after a set-field commits |

The two hooks fire once per affected row, after the per-model transaction
has committed. They run outside the transaction, so a hook panic does
not roll back the write. A panicking hook is also recovered and logged
(with a stack trace); it does not strand the model's remaining hooks,
abort later models, or kill the background loop — the sweep continues.

### What one tick does

For each registered model with scheduled specs, in turn:

1. Run a `SELECT id, <column>, <conditional fields> FROM <table>
    WHERE <column> <= now() AND ...` to find rows due for action.
   The `from=` qualifier becomes an additional `AND field = 'value'`
   clause.
2. Open a per-model transaction.
3. Apply the action to each row in the batch:
   - `soft-delete` → `UPDATE table SET deleted_at = now() WHERE id = ?`
   - `hard-delete` → `DELETE FROM table WHERE id = ?` (via the adapter's
     `HardDelete` if available)
   - `field=NAME;to=VALUE` → `UPDATE table SET name = ? WHERE id = ?`
4. Commit the transaction.
5. Fire `OnDelete` / `OnSetField` hooks for each row, in order.
6. Move to the next model.

A row an action can no longer touch — already deleted this tick by a
prior spec on the same row, already soft-deleted, or removed by a
concurrent replica — matches zero rows. That is an idempotent no-op, not
a failure: the row is skipped (counted in `Report.Skipped`) and the batch
continues. Without this, a same-row `hard-delete` + `set-field` would
delete the row, fail the follow-up update, roll the whole batch back, and
re-read the identical rows next tick — starving the model forever.

The per-model transaction means a single genuinely bad row aborts only
that model's batch, not the whole sweep. Errors are appended to the
tick's `Report.Errors` and logged. A panic inside a model's sweep (from
an adapter, `MapToRecord`, or a transaction op) is contained the same
way: it is recovered into a `Report.Errors` entry, the transaction rolls
back, and the remaining models are still swept.

### `Sweep` for one-shot ticks

`runner.Sweep(ctx)` runs exactly one tick and returns the `Report`:

```go
report, err := runner.Sweep(ctx)
log.Printf("deleted %d, updated %d across %d models",
    report.Deleted, report.Updated, len(report.PerModel))
```

Useful in tests and for cron-driven deployments where the framework's
internal ticker is the wrong fit. `Sweep` blocks until the pass completes.

## Distributed runners

A single runner per cluster is enough for most workloads — the operations
it performs (soft-delete, status flip) are idempotent. Two runners
processing the same batch simultaneously would do redundant work but no
incorrect work.

For at-most-once semantics in a multi-process deployment, run the
runner in only one replica (a leader-elected pod, a sidecar, a separate
deployment). Or use the `scheduled/jobsx` adapter, which bridges the
runner to a `jobs` queue so the sweep is enqueued as a durable job and
dispatched by the worker pool:

```go
import (
    "time"

    "github.com/xaleel/maniflex/jobs"
    "github.com/xaleel/maniflex/jobs/cron"
    "github.com/xaleel/maniflex/scheduled"
    "github.com/xaleel/maniflex/scheduled/jobsx"
)

// Register the sweep handler on the worker, keyed by jobsx.JobType
// ("maniflex.scheduled.sweep").
w, _ := jobs.NewWorker(jobs.WorkerConfig{
    Source:   queue.(jobs.Source),
    Handlers: map[string]jobs.Handler{jobsx.JobType: jobsx.JobHandler(runner)},
})
go w.Run(ctx)

// A fixed-interval ticker enqueues one sweep job per minute; exactly one worker
// picks up any given tick.
sched := cron.New(queue, nil)
sched.Add(cron.Entry{Every: time.Minute, Job: jobs.Job{Type: jobsx.JobType}})
sched.Start(ctx)
```

In this setup the ticker drives the queue, not the runner directly —
exactly one worker processes any given tick, even with many app replicas.

## Hooks for events and audit

`OnDelete` and `OnSetField` are the natural place to emit events for
scheduled transitions, so downstream systems learn that a row's status
changed even though no HTTP request caused the change:

```go
runner, _ := scheduled.New(server, scheduled.Config{
    OnSetField: func(model, id, field, to string) {
        data, _ := json.Marshal(map[string]any{
            "model": model, "id": id, "field": field, "to": to,
        })
        _ = bus.Publish(context.Background(), events.Event{
            Type: "scheduled-transition",
            Data: data, // Event.Data is json.RawMessage
        })
    },
})
```

The hook fires outside the database transaction. For at-least-once
delivery semantics, write a row to an outbox table from inside the
runner's transaction (via a custom DB middleware on the affected models)
rather than relying on the hook.

## Interaction with versioning and audit

A scheduled transition is just an `UPDATE` (or `DELETE`) issued by the
runner. It flows through the model's normal middleware:

- `Versioned` models get a history row for the transition, with
  `actor_id = NULL` (no `ctx.Auth` exists in the runner).
- `db.AuditLog` records the write the same way.

This is intentional — a status change is a status change, regardless of
whether a human or the runner triggered it.

## When to use scheduled fields

| Need | Fit |
|---|---|
| Auto-publish at a fixed time | yes |
| Auto-archive / auto-expire | yes |
| Soft-delete on retention deadline | yes |
| Send an email at 9 AM tomorrow | not directly — use a job queue; the runner only mutates rows |
| Run a multi-step workflow at a deadline | not directly — hook into `OnSetField` to enqueue the workflow |

The runner is deliberately simple: timestamp + row-local change. For
side-effecting work outside the database, use it as a *trigger* and
delegate the actual work to a job queue.

## Operational checklist

- One runner per cluster, started once, stopped on shutdown.
- Set `Interval` to the desired granularity — `1m` is plenty for most
  workflows; tighten if you have sub-minute deadlines.
- Set `BatchSize` to a value the database can absorb in one transaction
  without blocking writers. 500 is a safe default; for very high-volume
  tables tune lower so each batch is shorter.
- Use `OnDelete` / `OnSetField` hooks for observability — emit events,
  increment metrics, log structured records.
- For deployments with multiple app replicas, gate the runner to one
  process or use `scheduled/jobsx` to dispatch sweeps through your job
  queue.
- Combine with `maniflex.WithDeletedAt` for the soft-delete-on-expiry pattern;
  the indexed `deleted_at IS NULL` predicate keeps the sweep query
  cheap as the table grows.
