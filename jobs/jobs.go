// Package jobs provides a durable, adapter-pluggable job queue for maniflex.
//
// The core package has no third-party imports. Choose an adapter sub-package
// for the backing store:
//
//   - jobs/inproc — goroutine pool; for tests and single-binary apps
//   - jobs/sql    — Postgres/SQLite with transactional outbox semantics
//   - jobs/redis  — Redis Streams; for high-throughput worker fleets
//
// A scheduled trigger is available in jobs/cron. Status persistence for the
// REST layer lives in jobs/maniflex (3C.4).
package jobs

import (
	"context"
	"encoding/json"
	"time"
)

// Status describes the lifecycle state of a job.
type Status string

const (
	StatusEnqueued  Status = "enqueued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed" // retryable terminal
	StatusDead      Status = "dead"   // permanent — MaxRetry exhausted
	StatusCancelled Status = "cancelled"
)

// Job is the unit of work passed to a Handler. All fields except Type are
// optional; adapters supply sensible defaults on Enqueue.
type Job struct {
	// ID is a ULID-style sortable identifier. Auto-assigned on Enqueue when empty.
	ID string

	// Type identifies the handler to dispatch to (e.g. "export_invoices").
	Type string

	// Payload is the opaque, handler-specific data. Handlers decode it themselves.
	Payload json.RawMessage

	// TraceID is the W3C traceparent value propagated to the Handler context.
	TraceID string

	ActorID  string
	TenantID string

	// MaxRetry is the maximum number of attempts before the job is marked dead.
	// Zero means the adapter default (3).
	MaxRetry int

	// Backoff controls the delay between retries. nil uses ExponentialBackoff{1s, 5m}.
	Backoff BackoffPolicy

	// Priority is a hint to the adapter; higher values run first.
	Priority int

	// NotBefore is the earliest time the job may be executed. Zero means now.
	NotBefore time.Time

	// GroupKey serialises execution: at most one job per key runs at a time.
	// e.g. tenant_id for per-tenant payroll runs.
	GroupKey string

	// Headers are arbitrary routing hints or idempotency keys.
	Headers map[string]string

	// Attempts is populated by the Source on Dequeue. Handlers and the Worker
	// use it to compute backoff delay; it is not set by the caller.
	Attempts int
}

// Result is the structured output of a successful Handler invocation.
type Result struct {
	// Output is a small, structured result (stored inline).
	Output json.RawMessage

	// URL is a pre-signed or server-relative URL to a large output artefact.
	URL string

	// Mime is the content-type of the artefact at URL.
	Mime string

	SizeBytes int64
}

// StatusInfo carries supplemental data for a StatusSink.Transition call.
type StatusInfo struct {
	Attempt  int
	Error    string
	Result   *Result
	JobType  string
	ActorID  string
	TenantID string
}

// JobState is the full runtime representation of a job, including persistence metadata.
type JobState struct {
	Job
	Status      Status
	Error       string
	StartedAt   *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ── Interfaces ────────────────────────────────────────────────────────────────

// Queue is the producer surface. All three methods are safe for concurrent use.
type Queue interface {
	// Enqueue submits j for immediate execution. The returned id is the assigned
	// Job.ID (auto-generated if j.ID is empty).
	Enqueue(ctx context.Context, j Job) (id string, err error)

	// EnqueueAt submits j for execution no earlier than at.
	EnqueueAt(ctx context.Context, j Job, at time.Time) (id string, err error)

	// EnqueueBatch submits multiple jobs atomically where the adapter supports it.
	// Returns one id per job in input order.
	EnqueueBatch(ctx context.Context, js []Job) ([]string, error)

	Close() error
}

// Source is the consumer surface, implemented by adapters. The Worker calls
// these methods; application code does not call them directly.
type Source interface {
	// Dequeue claims up to n ready jobs and returns them. It sets Job.Attempts
	// to the current attempt count for each returned job.
	// Blocks briefly (adapter-defined) when no jobs are ready; returns an empty
	// slice (not an error) on timeout.
	Dequeue(ctx context.Context, n int) ([]Job, error)

	// Ack marks id as succeeded.
	Ack(ctx context.Context, id string) error

	// Nack marks id as failed. If the job's attempt count has reached MaxRetry
	// the adapter marks it dead; otherwise it reschedules with the given delay.
	Nack(ctx context.Context, id string, jobErr error, delay time.Duration) error

	// Dead unconditionally marks id as dead (permanent failure).
	Dead(ctx context.Context, id string, jobErr error) error
}

// BlockingSource is an optional capability adapters may implement to avoid
// poll-and-sleep behaviour during idle periods. When the Worker has free
// slots and no other jobs to dispatch it calls DequeueBlocking, which is
// expected to wait — up to `max` — until at least one job becomes available
// (Redis BRPOP, Postgres LISTEN/NOTIFY, ...) or the context is cancelled.
//
// Adapters that do not implement BlockingSource fall back to the standard
// poll cycle controlled by WorkerConfig.EmptyQueueBackoff. There is no
// downside to implementing both; the worker prefers the blocking variant
// whenever the slot pool is fully idle so it does not hammer the source.
type BlockingSource interface {
	Source
	// DequeueBlocking is like Dequeue but waits up to max for at least one
	// job to become available. Returns the empty slice (not an error) on
	// timeout or context cancellation. Implementations must respect ctx.
	DequeueBlocking(ctx context.Context, n int, max time.Duration) ([]Job, error)
}

// Requeuer is an optional capability for returning a job to the queue WITHOUT
// counting the delivery as a retry attempt. The Worker uses it for a job whose
// type it has no handler for: the job is requeued so a differently-configured
// worker can claim it (e.g. one still starting during a deploy), rather than
// failed against its retry budget.
//
// Adapters that do not implement it fall back to Nack, which does spend the
// budget and, on some adapters, requeues without bound — so implementing
// Requeuer is what lets the Worker bound the requeue and dead-letter a type no
// worker handles instead of storming (audit JB-4, JB-9).
type Requeuer interface {
	// Requeue re-persists j — including Header changes the Worker made, such as
	// HeaderUnhandledRequeues — and schedules it to become claimable again after
	// delay. It must store j.Attempts as given and must not increment it: an
	// unhandled delivery is not an attempt.
	Requeue(ctx context.Context, j Job, delay time.Duration) error
}

// HeaderUnhandledRequeues is the Job header in which the Worker counts how many
// times a job has been requeued for lack of a handler. When it reaches
// WorkerConfig.MaxUnhandledRequeues the job is dead-lettered instead of requeued
// again, so a type no worker handles surfaces rather than bouncing forever.
const HeaderUnhandledRequeues = "maniflex-unhandled-requeues"

// Handler is the function signature for job processing logic.
type Handler func(ctx context.Context, j Job) (Result, error)

// StatusSink receives lifecycle transitions from the Worker. Implement this to
// persist job status alongside your business data. jobs/maniflex provides one backed
// by the Server model layer.
type StatusSink interface {
	Transition(ctx context.Context, id string, from, to Status, info StatusInfo) error
}

// Cancellable is an optional capability adapters may implement.
type Cancellable interface {
	Cancel(ctx context.Context, id string) error
}

// Inspector is an optional capability adapters may implement for job introspection.
type Inspector interface {
	Get(ctx context.Context, id string) (JobState, error)
	List(ctx context.Context, q ListQuery) ([]JobState, error)
}

// ListQuery filters results for Inspector.List.
type ListQuery struct {
	Type     string
	Status   Status
	TenantID string
	ActorID  string
	Limit    int
	Offset   int
}
