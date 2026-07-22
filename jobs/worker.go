package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
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

	// MaxUnhandledRequeues bounds how many times a job of a type this worker has
	// no handler for is requeued before it is dead-lettered. Default: 20.
	//
	// Requeuing lets another worker — one still coming up during a deploy —
	// claim the job; the bound stops a type that NO worker handles from bouncing
	// between workers forever. It applies only when the Source implements
	// Requeuer; otherwise the worker falls back to Nack and cannot bound it.
	MaxUnhandledRequeues int

	// OnPanic is called when a handler panics. When nil the panic is logged and
	// the job is nacked.
	OnPanic func(j Job, recovered any)

	// EventBus is an optional bus for publishing job.{type}.completed and
	// job.{type}.failed events. Must implement EventPublisher if non-nil.
	// The worker checks for the interface at runtime; supplying a concrete type
	// avoids a compile-time import of the events package.
	EventBus any
}

// unhandledRequeueDelay is how long a job of an unhandled type waits before it
// becomes claimable again, so requeuing it doesn't hot-loop while a worker that
// handles the type picks it up. Defaults to 5s (or EmptyQueueBackoff if larger).
func (w *Worker) unhandledRequeueDelay() time.Duration {
	const d = 5 * time.Second
	if w.cfg.EmptyQueueBackoff > d {
		return w.cfg.EmptyQueueBackoff
	}
	return d
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
	if cfg.MaxUnhandledRequeues <= 0 {
		cfg.MaxUnhandledRequeues = 20
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
		w.requeueUnhandled(ctx, j, logger)
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

	// The handler has committed to an outcome; the bookkeeping that records it
	// must run on a context detached from ctx's cancellation. At shutdown ctx is
	// cancelled, and an Ack that fails because of it leaves a job that already
	// succeeded unacknowledged — so it is redelivered and runs a second time
	// (audit JB-5). The context is still bounded, so a hung backend cannot stall
	// shutdown, and it keeps ctx's trace values.
	termCtx, termCancel := w.terminalContext(ctx)
	defer termCancel()

	if err != nil {
		logger.Warn("[jobs] handler failed",
			slog.String("error", err.Error()),
			slog.Int("attempts", j.Attempts),
			slog.Int("max_retry", MaxRetryFor(j)),
		)
		if j.Attempts >= MaxRetryFor(j) {
			w.markDead(termCtx, j, err)
		} else {
			delay := BackoffFor(j).Next(j.Attempts)
			w.transition(termCtx, j, StatusRunning, StatusFailed, StatusInfo{
				Attempt:  j.Attempts,
				Error:    err.Error(),
				JobType:  j.Type,
				ActorID:  j.ActorID,
				TenantID: j.TenantID,
			})
			if nackErr := w.cfg.Source.Nack(termCtx, j.ID, err, delay); nackErr != nil {
				logger.Error("[jobs] nack failed", slog.String("error", nackErr.Error()))
			}
		}
		return
	}

	w.transition(termCtx, j, StatusRunning, StatusSucceeded, StatusInfo{
		Attempt:  j.Attempts,
		Result:   &result,
		JobType:  j.Type,
		ActorID:  j.ActorID,
		TenantID: j.TenantID,
	})
	w.publishJobEvent(termCtx, j, "completed")
	if ackErr := w.cfg.Source.Ack(termCtx, j.ID); ackErr != nil {
		logger.Error("[jobs] ack failed", slog.String("error", ackErr.Error()))
	}
}

// terminalWriteTimeout bounds the detached bookkeeping that finalises a job
// (Ack, Nack, dead-letter, status transition). Long enough for a single backend
// write to complete during shutdown, short enough that a hung backend cannot
// hold the worker's Shutdown open beyond it.
const terminalWriteTimeout = 30 * time.Second

// terminalContext derives the context used to finalise a job's outcome. It is
// detached from ctx's cancellation — the finalising write must not be abandoned
// just because the worker is stopping — but bounded and value-preserving. See
// the call site in handle for why (audit JB-5).
func (w *Worker) terminalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
}

// requeueUnhandled handles a job whose type this worker has no handler for. It
// requeues the job so another worker can claim it — one still coming up during
// a deploy, say — but bounds that with a per-job counter (HeaderUnhandledRequeues)
// so a type NO worker handles is dead-lettered instead of bouncing forever
// (audit JB-4). The requeue does not spend a retry attempt, which the retry
// budget must not be (audit JB-9).
//
// It requeues (or dead-letters) on a terminalContext — detached from the run
// context's cancellation so the durable write completes even as the worker
// shuts down, or the job would be left stuck running (audit JB-4/JB-5).
func (w *Worker) requeueUnhandled(ctx context.Context, j Job, logger *slog.Logger) {
	err := fmt.Errorf("no handler registered for job type %q", j.Type)
	termCtx, termCancel := w.terminalContext(ctx)
	defer termCancel()

	rq, ok := w.cfg.Source.(Requeuer)
	if !ok {
		// The Source cannot re-persist the counter, so the requeue cannot be
		// bounded here. Preserve the legacy Nack behaviour rather than silently
		// changing it for third-party adapters.
		logger.Warn("[jobs] requeuing job of unhandled type (source has no Requeuer; requeue is unbounded)",
			slog.String("type", j.Type), slog.String("job_id", j.ID))
		if nackErr := w.cfg.Source.Nack(termCtx, j.ID, err, w.unhandledRequeueDelay()); nackErr != nil {
			logger.Error("[jobs] requeue of unhandled type failed",
				slog.String("job_id", j.ID), slog.String("error", nackErr.Error()))
			return // the job is still held; a running status row is the truth
		}
		// This branch used to leave the status row on running while the job was
		// back in the queue, so it read as executing forever on a worker that had
		// already let it go (audit JB-18). Nack, unlike Requeue, does spend the
		// attempt, so the row follows what Nack itself does with the budget —
		// the same test handle() applies to a failed handler.
		to := StatusFailed
		if j.Attempts >= MaxRetryFor(j) {
			to = StatusDead
		}
		w.transition(termCtx, j, StatusRunning, to, StatusInfo{
			Attempt: j.Attempts, Error: err.Error(),
			JobType: j.Type, ActorID: j.ActorID, TenantID: j.TenantID,
		})
		return
	}

	count := unhandledCount(j)
	if count >= w.cfg.MaxUnhandledRequeues {
		logger.Error("[jobs] dead-lettering job no worker handles",
			slog.String("type", j.Type), slog.String("job_id", j.ID),
			slog.Int("requeues", count))
		w.markDead(termCtx, j, err)
		return
	}

	// Undo the attempt this delivery counted, so shuttling a job past a worker
	// that cannot handle it does not erode the budget a real handler will need.
	if j.Attempts > 0 {
		j.Attempts--
	}
	j.Headers = withUnhandledCount(j.Headers, count+1)

	logger.Warn("[jobs] requeuing job of unhandled type",
		slog.String("type", j.Type), slog.String("job_id", j.ID),
		slog.Int("requeue", count+1), slog.Int("max", w.cfg.MaxUnhandledRequeues))

	// The queue is the source of truth, so it moves first and the status row
	// follows. The other order wrote "enqueued" before the requeue was durable,
	// so a failed Requeue left the status row describing a job the queue still
	// holds as running. Requeuing with a delay (5s or more) means no other worker
	// can claim it and re-transition the row in between.
	if rqErr := rq.Requeue(termCtx, j, w.unhandledRequeueDelay()); rqErr != nil {
		logger.Error("[jobs] requeue of unhandled type failed",
			slog.String("job_id", j.ID), slog.String("error", rqErr.Error()))
		return // still held by the queue; running is the honest status
	}

	// It is back in the queue, not running — move the status row off running so
	// it does not read as executing forever (audit JB-18).
	w.transition(termCtx, j, StatusRunning, StatusEnqueued, StatusInfo{
		Attempt: j.Attempts, JobType: j.Type, ActorID: j.ActorID, TenantID: j.TenantID,
	})
}

// unhandledCount reads the requeue counter carried in the job's headers.
func unhandledCount(j Job) int {
	if j.Headers == nil {
		return 0
	}
	n, _ := strconv.Atoi(j.Headers[HeaderUnhandledRequeues])
	if n < 0 {
		return 0
	}
	return n
}

// withUnhandledCount returns a copy of h with the requeue counter set to n. It
// copies rather than mutating so the caller's job (and any retained reference)
// is not changed underneath it.
func withUnhandledCount(h map[string]string, n int) map[string]string {
	out := make(map[string]string, len(h)+1)
	for k, v := range h {
		out[k] = v
	}
	out[HeaderUnhandledRequeues] = strconv.Itoa(n)
	return out
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
	// ctx is a terminalContext at both call sites: detached from the run
	// context's cancellation so dead-lettering completes during shutdown
	// (audit JB-5), but bounded.
	if deadErr := w.cfg.Source.Dead(ctx, j.ID, err); deadErr != nil {
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
	if _, enqErr := q.Enqueue(ctx, dlq); enqErr != nil {
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
