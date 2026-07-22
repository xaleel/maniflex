package e2e

// Worker behaviour driven entirely through jobs.WorkerConfig and the Source
// interfaces — the seam an application actually configures. Nothing here reaches
// into the jobs package; each test supplies a fake Source with exactly the
// optional capabilities under test (LeaseRenewer, BlockingSource, Queue for DLQ
// routing) and asserts what the Worker does with it.
//
//	go test ./e2e/ -run TestWorkerSeam

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// seamSource hands out a fixed set of jobs once and records every settlement.
// Embedding it gives a test the base Source; adding a method to the wrapper is
// how each optional capability is opted into.
type seamSource struct {
	mu sync.Mutex

	pending  []jobs.Job
	acked    []string
	nacked   []string
	deaded   []string
	enqueued []jobs.Job // DLQ re-enqueues land here

	settled chan struct{}
	once    sync.Once
}

func newSeamSource(js ...jobs.Job) *seamSource {
	return &seamSource{pending: js, settled: make(chan struct{})}
}

func (s *seamSource) Dequeue(context.Context, int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil, nil
	}
	out := s.pending
	s.pending = nil
	for i := range out {
		out[i].Attempts++
	}
	return out, nil
}

func (s *seamSource) Ack(_ context.Context, id string) error {
	s.mu.Lock()
	s.acked = append(s.acked, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *seamSource) Nack(_ context.Context, id string, _ error, _ time.Duration) error {
	s.mu.Lock()
	s.nacked = append(s.nacked, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *seamSource) Dead(_ context.Context, id string, _ error) error {
	s.mu.Lock()
	s.deaded = append(s.deaded, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *seamSource) signal() { s.once.Do(func() { close(s.settled) }) }

func (s *seamSource) snapshot() (acked, nacked, deaded []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acked...), append([]string(nil), s.nacked...), append([]string(nil), s.deaded...)
}

// dlqSource additionally implements jobs.Queue, which is what markDead requires
// before it will route a dead job to the DLQ type.
type dlqSource struct{ *seamSource }

func (s *dlqSource) Enqueue(_ context.Context, j jobs.Job) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, j)
	return "dlq-1", nil
}
func (s *dlqSource) EnqueueAt(ctx context.Context, j jobs.Job, _ time.Time) (string, error) {
	return s.Enqueue(ctx, j)
}
func (s *dlqSource) EnqueueBatch(context.Context, []jobs.Job) ([]string, error) { return nil, nil }
func (s *dlqSource) Close() error                                               { return nil }

// renewSource implements jobs.LeaseRenewer and counts renewals.
type renewSource struct {
	*seamSource
	mu       sync.Mutex
	renewals []time.Duration
}

func (s *renewSource) RenewLease(_ context.Context, _ string, d time.Duration) error {
	s.mu.Lock()
	s.renewals = append(s.renewals, d)
	s.mu.Unlock()
	return nil
}

func (s *renewSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.renewals)
}

// blockingSource implements jobs.BlockingSource, the long-poll variant the
// Worker prefers while the pool is idle.
type blockingSource struct {
	*seamSource
	mu     sync.Mutex
	blocks int
}

func (s *blockingSource) DequeueBlocking(ctx context.Context, n int, _ time.Duration) ([]jobs.Job, error) {
	s.mu.Lock()
	s.blocks++
	s.mu.Unlock()
	return s.Dequeue(ctx, n)
}

func (s *blockingSource) blockCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocks
}

// startSeamWorker runs a Worker in the background and returns a stop func that
// cancels it and waits for Run to return.
func startSeamWorker(t *testing.T, cfg jobs.WorkerConfig) (*jobs.Worker, func()) {
	t.Helper()
	w, err := jobs.NewWorker(cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	return w, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker Run did not return after cancellation")
		}
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ── Shutdown ──────────────────────────────────────────────────────────────────

// Shutdown's whole purpose: a handler already running gets to finish. Cancelling
// the run context stops new work, and Shutdown waits out what is in flight.
func TestWorkerSeam_ShutdownWaitsForInFlightHandlers(t *testing.T) {
	src := newSeamSource(jobs.Job{ID: "j1", Type: "slow", MaxRetry: 3})
	entered := make(chan struct{})
	finished := make(chan struct{})

	w, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"slow": func(context.Context, jobs.Job) (jobs.Result, error) {
				close(entered)
				time.Sleep(150 * time.Millisecond)
				close(finished)
				return jobs.Result{}, nil
			},
		},
	})

	awaitClosed(t, entered, "the handler to start")
	stop() // cancel the run context while the handler is mid-flight

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case <-finished:
	default:
		t.Fatal("Shutdown returned while the handler was still running")
	}
	if acked, _, _ := src.snapshot(); len(acked) != 1 {
		t.Errorf("acked %v, want the in-flight job acknowledged before shutdown completed", acked)
	}
}

// A handler that will not return must not hold Shutdown open past its deadline,
// or a stuck job becomes a stuck deploy.
func TestWorkerSeam_ShutdownReportsItsDeadline(t *testing.T) {
	src := newSeamSource(jobs.Job{ID: "j1", Type: "stuck", MaxRetry: 3})
	entered := make(chan struct{})
	release := make(chan struct{})

	w, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"stuck": func(context.Context, jobs.Job) (jobs.Result, error) {
				close(entered)
				<-release
				return jobs.Result{}, nil
			},
		},
	})
	defer func() { close(release); stop() }()

	awaitClosed(t, entered, "the handler to start")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := w.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown reported success while a handler was still stuck")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// With nothing running, Shutdown returns at once.
func TestWorkerSeam_ShutdownOnAnIdleWorkerReturnsImmediately(t *testing.T) {
	src := newSeamSource()
	w, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       2,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"never": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	})
	stop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown of an idle worker: %v", err)
	}
}

// ── lease renewal ─────────────────────────────────────────────────────────────

// A Source implementing LeaseRenewer must have a long job's lease renewed while
// it runs — that is what keeps jobs/sql's reclaim sweep and jobs/redis's
// XAUTOCLAIM from treating a live worker as a dead one.
func TestWorkerSeam_LongHandlerHasItsLeaseRenewed(t *testing.T) {
	src := &renewSource{seamSource: newSeamSource(jobs.Job{ID: "j1", Type: "long", MaxRetry: 3})}
	release := make(chan struct{})

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		LeaseRenew:        20 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"long": func(context.Context, jobs.Job) (jobs.Result, error) {
				<-release
				return jobs.Result{}, nil
			},
		},
	})
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for src.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if src.count() < 2 {
		t.Fatalf("lease renewed %d times during a long handler, want repeated renewals", src.count())
	}

	// The renewal horizon must exceed the tick, or every renewal would expire
	// before the next one lands.
	src.mu.Lock()
	horizon := src.renewals[0]
	src.mu.Unlock()
	if horizon <= 20*time.Millisecond {
		t.Errorf("renewal horizon %v is not longer than the %v renew interval", horizon, 20*time.Millisecond)
	}

	close(release)
	awaitClosed(t, src.settled, "the job to settle")

	// Renewal must stop once the handler is done, or a finished job keeps
	// extending a lease nobody holds.
	after := src.count()
	time.Sleep(100 * time.Millisecond)
	if grew := src.count() - after; grew > 1 {
		t.Errorf("lease renewed %d more times after the handler finished", grew)
	}
}

// ── panic recovery ────────────────────────────────────────────────────────────

// A panicking handler must not take the worker down, and must be reported.
func TestWorkerSeam_HandlerPanicIsRecoveredAndReported(t *testing.T) {
	src := newSeamSource(jobs.Job{ID: "j1", Type: "boom", MaxRetry: 3})
	var (
		mu        sync.Mutex
		panicked  []string
		recovered any
	)

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		OnPanic: func(j jobs.Job, r any) {
			mu.Lock()
			panicked = append(panicked, j.ID)
			recovered = r
			mu.Unlock()
		},
		Handlers: map[string]jobs.Handler{
			"boom": func(context.Context, jobs.Job) (jobs.Result, error) {
				panic("handler exploded")
			},
		},
	})
	defer stop()

	awaitClosed(t, src.settled, "the panicking job to settle")

	mu.Lock()
	defer mu.Unlock()
	if len(panicked) != 1 || panicked[0] != "j1" {
		t.Fatalf("OnPanic saw %v, want [j1]", panicked)
	}
	if recovered != "handler exploded" {
		t.Errorf("recovered value = %v, want the panic argument", recovered)
	}
	// A panic is a failure with budget left, so it retries rather than dying.
	_, nacked, deaded := src.snapshot()
	if len(nacked) != 1 {
		t.Errorf("nacked %v, want the panicking job retried", nacked)
	}
	if len(deaded) != 0 {
		t.Errorf("dead-lettered %v on the first panic, want a retry", deaded)
	}
}

// The worker must keep serving after a panic, not wedge on the recovered slot.
func TestWorkerSeam_WorkerSurvivesAPanicAndKeepsWorking(t *testing.T) {
	src := newSeamSource(
		jobs.Job{ID: "j1", Type: "boom", MaxRetry: 3},
		jobs.Job{ID: "j2", Type: "fine", MaxRetry: 3},
	)
	done := make(chan struct{})

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"boom": func(context.Context, jobs.Job) (jobs.Result, error) { panic("boom") },
			"fine": func(context.Context, jobs.Job) (jobs.Result, error) {
				close(done)
				return jobs.Result{}, nil
			},
		},
	})
	defer stop()

	awaitClosed(t, done, "the job after the panicking one to run")
}

// ── dead-letter routing ───────────────────────────────────────────────────────

// With DLQType set and a handler registered for it, a job that exhausts its
// budget is re-enqueued under that type carrying the original as payload.
func TestWorkerSeam_ExhaustedJobIsRoutedToTheDLQType(t *testing.T) {
	src := &dlqSource{seamSource: newSeamSource(
		// Delivery makes Attempts 1, which equals MaxRetry: this failure is terminal.
		jobs.Job{ID: "j1", Type: "doomed", MaxRetry: 1, Payload: json.RawMessage(`{"n":7}`)},
	)}

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		DLQType:           "dlq",
		Handlers: map[string]jobs.Handler{
			"doomed": func(context.Context, jobs.Job) (jobs.Result, error) {
				return jobs.Result{}, errors.New("always fails")
			},
			"dlq": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	})
	defer stop()

	awaitClosed(t, src.settled, "the doomed job to be dead-lettered")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		src.mu.Lock()
		n := len(src.enqueued)
		src.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.enqueued) != 1 {
		t.Fatalf("re-enqueued %d jobs, want 1 routed to the DLQ type", len(src.enqueued))
	}
	dlq := src.enqueued[0]
	if dlq.Type != "dlq" {
		t.Errorf("DLQ job type = %q, want %q", dlq.Type, "dlq")
	}
	if dlq.ID != "" {
		t.Errorf("DLQ job kept id %q; it must get a fresh one", dlq.ID)
	}
	var payload map[string]any
	if err := json.Unmarshal(dlq.Payload, &payload); err != nil {
		t.Fatalf("DLQ payload is not JSON: %v", err)
	}
	for _, k := range []string{"original_type", "original_id", "original_payload", "error"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("DLQ payload is missing %q — the original is unrecoverable: %v", k, payload)
		}
	}
	if payload["original_type"] != "doomed" || payload["original_id"] != "j1" {
		t.Errorf("DLQ payload does not identify the original: %v", payload)
	}
}

// No DLQType means the job simply dies; nothing extra is enqueued.
func TestWorkerSeam_WithoutDLQTypeNothingIsReEnqueued(t *testing.T) {
	src := &dlqSource{seamSource: newSeamSource(
		jobs.Job{ID: "j1", Type: "doomed", MaxRetry: 1},
	)}

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"doomed": func(context.Context, jobs.Job) (jobs.Result, error) {
				return jobs.Result{}, errors.New("always fails")
			},
		},
	})
	defer stop()

	awaitClosed(t, src.settled, "the doomed job to be dead-lettered")
	time.Sleep(80 * time.Millisecond)

	src.mu.Lock()
	reEnqueued := len(src.enqueued)
	src.mu.Unlock() // snapshot takes the same lock; sync.Mutex is not reentrant

	if reEnqueued != 0 {
		t.Errorf("re-enqueued %d jobs with no DLQType configured", reEnqueued)
	}
	if _, _, deaded := src.snapshot(); len(deaded) != 1 {
		t.Errorf("deaded %v, want the exhausted job dead-lettered", deaded)
	}
}

// ── trace propagation ─────────────────────────────────────────────────────────

// The job's TraceID has to reach the handler's context, or a job's work cannot
// be correlated with the request that enqueued it.
func TestWorkerSeam_TraceIDReachesTheHandler(t *testing.T) {
	src := newSeamSource(jobs.Job{ID: "j1", Type: "traced", MaxRetry: 3, TraceID: "trace-abc"})
	seen := make(chan string, 1)

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"traced": func(ctx context.Context, _ jobs.Job) (jobs.Result, error) {
				seen <- jobs.TraceIDFromContext(ctx)
				return jobs.Result{}, nil
			},
		},
	})
	defer stop()

	select {
	case got := <-seen:
		if got != "trace-abc" {
			t.Fatalf("handler saw trace id %q, want %q", got, "trace-abc")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// A job with no TraceID must yield the empty string, not a stale one.
func TestWorkerSeam_NoTraceIDYieldsEmpty(t *testing.T) {
	src := newSeamSource(jobs.Job{ID: "j1", Type: "plain", MaxRetry: 3})
	seen := make(chan string, 1)

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"plain": func(ctx context.Context, _ jobs.Job) (jobs.Result, error) {
				seen <- jobs.TraceIDFromContext(ctx)
				return jobs.Result{}, nil
			},
		},
	})
	defer stop()

	select {
	case got := <-seen:
		if got != "" {
			t.Fatalf("handler saw trace id %q for a job carrying none", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// ── blocking source ───────────────────────────────────────────────────────────

// A Source implementing BlockingSource must be used for the long poll while the
// pool is idle — that is the whole point of the optional interface.
func TestWorkerSeam_BlockingSourceIsPreferredWhenIdle(t *testing.T) {
	src := &blockingSource{seamSource: newSeamSource(jobs.Job{ID: "j1", Type: "t", MaxRetry: 3})}

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"t": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	})
	defer stop()

	awaitClosed(t, src.settled, "the job to settle")
	if src.blockCount() == 0 {
		t.Fatal("DequeueBlocking was never called; the Worker ignored the BlockingSource")
	}
}

// ── backoff wiring ────────────────────────────────────────────────────────────

// Job.Backoff has to be the policy the Worker actually applies when it nacks.
func TestWorkerSeam_JobBackoffPolicyIsApplied(t *testing.T) {
	src := &delaySource{seamSource: newSeamSource(jobs.Job{
		ID: "j1", Type: "retry", MaxRetry: 3,
		Backoff: jobs.FixedBackoff{Delay: 1234 * time.Millisecond},
	})}

	_, stop := startSeamWorker(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"retry": func(context.Context, jobs.Job) (jobs.Result, error) {
				return jobs.Result{}, errors.New("nope")
			},
		},
	})
	defer stop()

	awaitClosed(t, src.settled, "the job to be nacked")

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.delays) != 1 {
		t.Fatalf("recorded %d nack delays, want 1", len(src.delays))
	}
	if src.delays[0] != 1234*time.Millisecond {
		t.Fatalf("nack delay = %v, want the job's FixedBackoff of %v", src.delays[0], 1234*time.Millisecond)
	}
}

// delaySource records the delay the Worker passes to Nack.
type delaySource struct {
	*seamSource
	mu     sync.Mutex
	delays []time.Duration
}

func (s *delaySource) Nack(ctx context.Context, id string, err error, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	return s.seamSource.Nack(ctx, id, err, d)
}
