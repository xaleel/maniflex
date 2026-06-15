package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// eventPublisher is a runtime interface check against WorkerConfig.EventBus
// to avoid a compile-time import of the events package.
type eventPublisher interface {
	Publish(ctx context.Context, eventType string, payload any) error
}

// WorkerConfig configures a Worker.
type WorkerConfig struct {
	// Source is the adapter the worker dequeues from. Required.
	Source Source

	// Handlers maps Job.Type to the processing function.
	Handlers map[string]Handler

	// Concurrency is the maximum number of jobs processed simultaneously.
	// Defaults to runtime.GOMAXPROCS(0).
	Concurrency int

	// Status receives lifecycle transitions (enqueued→running→succeeded|failed|dead).
	// nil disables status tracking.
	Status StatusSink

	Logger *slog.Logger

	// LeaseRenew is how often the worker renews the job lease for long-running
	// handlers. Only used by adapters that implement LeaseRenewer.
	// Defaults to 30s.
	LeaseRenew time.Duration

	// ShutdownWait is the maximum duration Shutdown() waits for in-flight
	// handlers to complete. Defaults to 30s.
	ShutdownWait time.Duration

	// EmptyQueueBackoff is how long the worker waits after Dequeue returns
	// no jobs before trying again. Pre-fix this was a hard-coded 100ms,
	// which hammered Redis BRPOP / Postgres SKIP LOCKED at scale. Default
	// 1s; the worker grows the delay exponentially (×2 per consecutive
	// empty) up to MaxEmptyQueueBackoff. Adapters that implement
	// BlockingSource bypass this entirely when the slot pool is fully idle.
	EmptyQueueBackoff time.Duration

	// MaxEmptyQueueBackoff caps the exponential backoff above. Default 30s.
	MaxEmptyQueueBackoff time.Duration

	// DLQType is the job type used for dead-letter routing. When non-empty and
	// a matching handler is registered, dead jobs are re-enqueued under this
	// type with the original job as payload.
	DLQType string

	// OnPanic is called when a handler panics. When nil the panic is logged and
	// the job is nacked.
	OnPanic func(j Job, recovered any)

	// EventBus is an optional bus for publishing job.{type}.completed and
	// job.{type}.failed events. Must implement EventPublisher if non-nil.
	// The worker checks for the interface at runtime; supplying a concrete type
	// avoids a compile-time import of the events package.
	EventBus any
}

// LeaseRenewer is an optional interface Source adapters may implement to allow
// the worker to extend a job's lease while it is running.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, id string, d time.Duration) error
}

// Worker dispatches jobs from a Source to the registered Handlers.
type Worker struct {
	cfg WorkerConfig
	sem chan struct{}
	wg  sync.WaitGroup
}

// NewWorker validates cfg and returns a ready Worker.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("jobs: WorkerConfig.Source must not be nil")
	}
	if len(cfg.Handlers) == 0 {
		return nil, fmt.Errorf("jobs: WorkerConfig.Handlers must not be empty")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = runtime.GOMAXPROCS(0)
	}
	if cfg.LeaseRenew <= 0 {
		cfg.LeaseRenew = 30 * time.Second
	}
	if cfg.ShutdownWait <= 0 {
		cfg.ShutdownWait = 30 * time.Second
	}
	if cfg.EmptyQueueBackoff <= 0 {
		cfg.EmptyQueueBackoff = time.Second
	}
	if cfg.MaxEmptyQueueBackoff <= 0 {
		cfg.MaxEmptyQueueBackoff = 30 * time.Second
	}
	if cfg.MaxEmptyQueueBackoff < cfg.EmptyQueueBackoff {
		cfg.MaxEmptyQueueBackoff = cfg.EmptyQueueBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{
		cfg: cfg,
		sem: make(chan struct{}, cfg.Concurrency),
	}, nil
}

// Run starts the dequeue-dispatch loop. It blocks until ctx is cancelled, then
// returns nil. Use Shutdown to drain in-flight handlers after cancellation.
//
// Loop shape:
//   - All slots busy: wait on the semaphore (no polling).
//   - Slots available: call Source.Dequeue, or DequeueBlocking when the
//     adapter implements BlockingSource AND the pool is fully idle (no
//     in-flight jobs). This trades a brief wakeup latency on warm-up for
//     zero idle traffic on cold paths.
//   - Empty result: exponential backoff from EmptyQueueBackoff to
//     MaxEmptyQueueBackoff, reset on the next successful dispatch.
func (w *Worker) Run(ctx context.Context) error {
	blockingSrc, _ := w.cfg.Source.(BlockingSource)
	emptyBackoff := w.cfg.EmptyQueueBackoff

	for {
		// Check for shutdown before attempting a dequeue.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// How many slots are free?
		free := cap(w.sem) - len(w.sem)
		if free == 0 {
			// All slots busy — block until one frees up (or ctx is cancelled).
			// We do this by taking a slot ourselves and immediately returning
			// it; the take blocks when the channel is full.
			select {
			case <-ctx.Done():
				return nil
			case w.sem <- struct{}{}:
				<-w.sem
			}
			continue
		}

		var jobs []Job
		var err error
		// Use the blocking variant only when the pool is fully idle — that's
		// the path where polling cost matters most. While jobs are running we
		// stick with non-blocking so the loop can react to slot churn quickly.
		if blockingSrc != nil && len(w.sem) == 0 {
			jobs, err = blockingSrc.DequeueBlocking(ctx, free, emptyBackoff)
		} else {
			jobs, err = w.cfg.Source.Dequeue(ctx, free)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.cfg.Logger.Error("[jobs] dequeue error", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}

		if len(jobs) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(emptyBackoff):
			}
			if emptyBackoff < w.cfg.MaxEmptyQueueBackoff {
				emptyBackoff *= 2
				if emptyBackoff > w.cfg.MaxEmptyQueueBackoff {
					emptyBackoff = w.cfg.MaxEmptyQueueBackoff
				}
			}
			continue
		}

		// A successful dispatch resets the backoff so a brief idle period
		// doesn't permanently slow down a busy queue.
		emptyBackoff = w.cfg.EmptyQueueBackoff

		for _, j := range jobs {
			w.sem <- struct{}{}
			w.wg.Add(1)
			go func(job Job) {
				defer w.wg.Done()
				defer func() { <-w.sem }()
				w.handle(ctx, job)
			}(j)
		}
	}
}

// Shutdown waits for all in-flight handlers to complete, bounded by the
// deadline on ctx. Returns an error if the deadline is exceeded.
func (w *Worker) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jobs: shutdown timed out: %w", ctx.Err())
	}
}

// handle executes one job, managing status transitions, lease renewal, panic
// recovery, and retry/dead routing.
func (w *Worker) handle(ctx context.Context, j Job) {
	logger := w.cfg.Logger.With(
		slog.String("job_id", j.ID),
		slog.String("job_type", j.Type),
		slog.Int("attempt", j.Attempts),
	)

	// Propagate trace context so downstream calls emit consistent trace IDs.
	if j.TraceID != "" {
		ctx = context.WithValue(ctx, traceIDKey{}, j.TraceID)
	}

	w.transition(ctx, j, StatusEnqueued, StatusRunning, StatusInfo{
		Attempt:  j.Attempts,
		JobType:  j.Type,
		ActorID:  j.ActorID,
		TenantID: j.TenantID,
	})

	h, ok := w.cfg.Handlers[j.Type]
	if !ok {
		err := fmt.Errorf("no handler registered for job type %q", j.Type)
		logger.Error("[jobs] " + err.Error())
		w.markDead(ctx, j, err)
		return
	}

	// Start lease renewal if the source supports it.
	var stopRenew context.CancelFunc
	if lr, ok := w.cfg.Source.(LeaseRenewer); ok {
		var renewCtx context.Context
		renewCtx, stopRenew = context.WithCancel(ctx)
		go w.renewLoop(renewCtx, lr, j.ID, logger)
	}

	result, err := w.runHandler(ctx, j, h, logger)

	if stopRenew != nil {
		stopRenew()
	}

	if err != nil {
		logger.Warn("[jobs] handler failed",
			slog.String("error", err.Error()),
			slog.Int("attempts", j.Attempts),
			slog.Int("max_retry", MaxRetryFor(j)),
		)
		if j.Attempts >= MaxRetryFor(j) {
			w.markDead(ctx, j, err)
		} else {
			delay := BackoffFor(j).Next(j.Attempts)
			w.transition(ctx, j, StatusRunning, StatusFailed, StatusInfo{
				Attempt:  j.Attempts,
				Error:    err.Error(),
				JobType:  j.Type,
				ActorID:  j.ActorID,
				TenantID: j.TenantID,
			})
			if nackErr := w.cfg.Source.Nack(ctx, j.ID, err, delay); nackErr != nil {
				logger.Error("[jobs] nack failed", slog.String("error", nackErr.Error()))
			}
		}
		return
	}

	w.transition(ctx, j, StatusRunning, StatusSucceeded, StatusInfo{
		Attempt:  j.Attempts,
		Result:   &result,
		JobType:  j.Type,
		ActorID:  j.ActorID,
		TenantID: j.TenantID,
	})
	w.publishJobEvent(ctx, j, "completed")
	if ackErr := w.cfg.Source.Ack(ctx, j.ID); ackErr != nil {
		logger.Error("[jobs] ack failed", slog.String("error", ackErr.Error()))
	}
}

func (w *Worker) runHandler(ctx context.Context, j Job, h Handler, logger *slog.Logger) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if w.cfg.OnPanic != nil {
				w.cfg.OnPanic(j, r)
			} else {
				logger.Error("[jobs] handler panicked", slog.Any("panic", r))
			}
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j)
}

func (w *Worker) markDead(ctx context.Context, j Job, err error) {
	w.transition(ctx, j, StatusRunning, StatusDead, StatusInfo{
		Attempt:  j.Attempts,
		Error:    err.Error(),
		JobType:  j.Type,
		ActorID:  j.ActorID,
		TenantID: j.TenantID,
	})
	w.publishJobEvent(ctx, j, "failed")
	if deadErr := w.cfg.Source.Dead(context.Background(), j.ID, err); deadErr != nil {
		w.cfg.Logger.Error("[jobs] dead failed",
			slog.String("job_id", j.ID),
			slog.String("error", deadErr.Error()),
		)
	}
	if w.cfg.DLQType == "" {
		return
	}
	if _, ok := w.cfg.Handlers[w.cfg.DLQType]; !ok {
		return
	}
	q, ok := w.cfg.Source.(Queue)
	if !ok {
		return
	}
	raw, _ := json.Marshal(map[string]any{
		"original_type":    j.Type,
		"original_id":      j.ID,
		"original_payload": j.Payload,
		"error":            err.Error(),
	})
	dlq := j
	dlq.ID = ""
	dlq.Type = w.cfg.DLQType
	dlq.Payload = json.RawMessage(raw)
	if _, enqErr := q.Enqueue(context.Background(), dlq); enqErr != nil {
		w.cfg.Logger.Error("[jobs] dlq enqueue failed",
			slog.String("job_id", j.ID),
			slog.String("error", enqErr.Error()),
		)
	}
}

func (w *Worker) publishJobEvent(ctx context.Context, j Job, suffix string) {
	pub, ok := w.cfg.EventBus.(eventPublisher)
	if !ok {
		return
	}
	_ = pub.Publish(ctx, "job."+j.Type+"."+suffix, map[string]any{
		"job_id":  j.ID,
		"type":    j.Type,
		"attempt": j.Attempts,
	})
}

func (w *Worker) transition(ctx context.Context, j Job, from, to Status, info StatusInfo) {
	if w.cfg.Status == nil {
		return
	}
	if err := w.cfg.Status.Transition(ctx, j.ID, from, to, info); err != nil {
		w.cfg.Logger.Error("[jobs] status transition failed",
			slog.String("job_id", j.ID),
			slog.String("from", string(from)),
			slog.String("to", string(to)),
			slog.String("error", err.Error()),
		)
	}
}

func (w *Worker) renewLoop(ctx context.Context, lr LeaseRenewer, id string, logger *slog.Logger) {
	ticker := time.NewTicker(w.cfg.LeaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := lr.RenewLease(ctx, id, w.cfg.LeaseRenew*3); err != nil {
				logger.Warn("[jobs] lease renewal failed",
					slog.String("job_id", id),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// traceIDKey is the context key for propagating W3C traceparent values.
type traceIDKey struct{}

// TraceIDFromContext returns the trace ID stored by the Worker, or "".
func TraceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}
