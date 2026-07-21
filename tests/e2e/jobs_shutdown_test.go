package e2e

// Audit JB-5: the worker's terminal bookkeeping — Ack on success, Nack on
// retry, dead-lettering, and the status transitions around them — ran on the
// run context. At shutdown that context is cancelled, so an Ack that fails
// because of it leaves a job that ALREADY SUCCEEDED unacknowledged: it is
// redelivered on restart and runs a second time. The failure is invisible until
// a deploy happens to cancel a worker in the instant between a handler
// returning and its Ack landing.
//
// The fix runs those writes on a context detached from the run context's
// cancellation (but bounded, so a hung backend cannot stall shutdown). These
// tests drive it by cancelling the run context from inside the handler, then
// checking the terminal call still saw a live context.
//
//	go test ./e2e/ -run TestShutdown

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// shutdownSource hands one job, then records the context each terminal call
// observed. A non-nil ctx error means the run context leaked into a finalising
// write — the JB-5 bug.
type shutdownSource struct {
	mu sync.Mutex

	jobType         string
	maxRetry        int
	deliverAttempts int

	handed bool

	ackCalled  bool
	ackCtxErr  error
	nackCalled bool
	nackCtxErr error
	deadCalled bool
	deadCtxErr error

	settled chan struct{}
	once    sync.Once
}

func (s *shutdownSource) Dequeue(_ context.Context, _ int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handed {
		return nil, nil
	}
	s.handed = true
	return []jobs.Job{{
		ID:       "j1",
		Type:     s.jobType,
		MaxRetry: s.maxRetry,
		Attempts: s.deliverAttempts,
	}}, nil
}

func (s *shutdownSource) Ack(ctx context.Context, _ string) error {
	s.mu.Lock()
	s.ackCalled = true
	s.ackCtxErr = ctx.Err()
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *shutdownSource) Nack(ctx context.Context, _ string, _ error, _ time.Duration) error {
	s.mu.Lock()
	s.nackCalled = true
	s.nackCtxErr = ctx.Err()
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *shutdownSource) Dead(ctx context.Context, _ string, _ error) error {
	s.mu.Lock()
	s.deadCalled = true
	s.deadCtxErr = ctx.Err()
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *shutdownSource) signal() { s.once.Do(func() { close(s.settled) }) }

// runCancellingWorker starts a worker whose single handler runs handlerBody,
// which is passed a cancel func for the run context so it can simulate shutdown
// arriving mid-handler. It waits until the source records a terminal call.
func runCancellingWorker(t *testing.T, src *shutdownSource, handlerBody func(cancel context.CancelFunc) (jobs.Result, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := jobs.NewWorker(jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			src.jobType: func(context.Context, jobs.Job) (jobs.Result, error) {
				return handlerBody(cancel)
			},
		},
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	go func() { _ = w.Run(ctx) }()

	select {
	case <-src.settled:
	case <-time.After(3 * time.Second):
		t.Fatal("no terminal call recorded within 3s — the job was never finalised")
	}
}

// The core JB-5 regression: a job that succeeds while the worker is shutting
// down must still be acked, or it re-runs.
func TestShutdown_SucceededJobIsAckedDespiteCancellation(t *testing.T) {
	src := &shutdownSource{jobType: "known", maxRetry: 3, deliverAttempts: 1, settled: make(chan struct{})}

	runCancellingWorker(t, src, func(cancel context.CancelFunc) (jobs.Result, error) {
		cancel()                  // shutdown arrives...
		return jobs.Result{}, nil // ...but the handler already finished
	})

	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.ackCalled {
		t.Fatal("the succeeded job was never acked; it would be redelivered and run again")
	}
	if src.ackCtxErr != nil {
		t.Errorf("Ack ran on a cancelled context (%v): the ack fails and the job re-runs on restart", src.ackCtxErr)
	}
}

// The retry path must survive shutdown too: a Nack on a cancelled context can
// fail to record the failure and backoff, so the job comes back immediately
// with the wrong status instead of after its delay.
func TestShutdown_RetryNackSurvivesCancellation(t *testing.T) {
	src := &shutdownSource{jobType: "known", maxRetry: 3, deliverAttempts: 1, settled: make(chan struct{})}

	runCancellingWorker(t, src, func(cancel context.CancelFunc) (jobs.Result, error) {
		cancel()
		return jobs.Result{}, context.DeadlineExceeded // a retryable handler error
	})

	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.nackCalled {
		t.Fatal("the failed job was never nacked")
	}
	if src.nackCtxErr != nil {
		t.Errorf("Nack ran on a cancelled context (%v): the retry/backoff is not recorded", src.nackCtxErr)
	}
}

// The dead-letter path is terminal in the strongest sense — if the Dead write is
// lost, the exhausted job is redelivered and burns another cycle.
func TestShutdown_DeadLetterSurvivesCancellation(t *testing.T) {
	// Attempts already at MaxRetry, so the handler error routes straight to dead.
	src := &shutdownSource{jobType: "known", maxRetry: 3, deliverAttempts: 3, settled: make(chan struct{})}

	runCancellingWorker(t, src, func(cancel context.CancelFunc) (jobs.Result, error) {
		cancel()
		return jobs.Result{}, context.DeadlineExceeded
	})

	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.deadCalled {
		t.Fatal("the exhausted job was never dead-lettered")
	}
	if src.deadCtxErr != nil {
		t.Errorf("Dead ran on a cancelled context (%v): the job is not finalised and re-runs", src.deadCtxErr)
	}
}

// Sanity: with no cancellation the terminal call obviously has a live context —
// this pins that the detached context is derived correctly (not permanently
// pre-cancelled), so the test above is not vacuous.
func TestShutdown_NormalCompletionAcksOnLiveContext(t *testing.T) {
	src := &shutdownSource{jobType: "known", maxRetry: 3, deliverAttempts: 1, settled: make(chan struct{})}

	runCancellingWorker(t, src, func(context.CancelFunc) (jobs.Result, error) {
		return jobs.Result{}, nil // do not cancel
	})

	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.ackCalled || src.ackCtxErr != nil {
		t.Errorf("normal completion: ackCalled=%v ctxErr=%v, want true/nil", src.ackCalled, src.ackCtxErr)
	}
}
