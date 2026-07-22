package e2e

// Audit JB-18: a job of a type the worker has no handler for went back to the
// queue, but its status row was left on 'running' — so it read as executing
// forever on a worker that had already let it go. The Requeuer path was fixed
// with JB-4; the Nack fallback taken by a Source without Requeuer was not, and
// that is the branch third-party adapters land in.
//
// The fallback now records what Nack itself does with the budget (failed, or
// dead once it is spent), and both paths write the status row only after the
// queue write succeeds — the queue is the source of truth, and 'running' is the
// honest status for a job the queue still holds.
//
//	go test ./e2e/ -run TestJobStatusStuck

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// recordingSink captures every transition the worker reports, so a test can ask
// what the status row would finally read.
type recordingSink struct {
	mu  sync.Mutex
	got []jobs.Status
}

func (s *recordingSink) Transition(_ context.Context, _ string, _, to jobs.Status, _ jobs.StatusInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, to)
	return nil
}

// final returns the last status written, i.e. what the row reads at rest.
func (s *recordingSink) final() jobs.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return ""
	}
	return s.got[len(s.got)-1]
}

// plainSource is a Source with no Requeuer, handing one job once. nackErr makes
// the Nack fail, to exercise the ordering guarantee.
type plainSource struct {
	mu      sync.Mutex
	job     jobs.Job
	handed  bool
	nackErr error
	settled chan struct{}
	once    sync.Once
}

func (s *plainSource) Dequeue(context.Context, int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handed {
		return nil, nil
	}
	s.handed = true
	j := s.job
	j.Attempts++ // an adapter stamps the attempt count on delivery
	return []jobs.Job{j}, nil
}

func (s *plainSource) Ack(context.Context, string) error { return nil }

func (s *plainSource) Nack(context.Context, string, error, time.Duration) error {
	defer s.signal()
	return s.nackErr
}

func (s *plainSource) Dead(context.Context, string, error) error {
	s.signal()
	return nil
}

func (s *plainSource) signal() { s.once.Do(func() { close(s.settled) }) }

// failingRequeuer implements Requeuer but its Requeue always fails.
type failingRequeuer struct {
	plainSource
}

func (s *failingRequeuer) Requeue(context.Context, jobs.Job, time.Duration) error {
	defer s.signal()
	return errors.New("queue unreachable")
}

func runUnhandled(t *testing.T, src jobs.Source, sink jobs.StatusSink, done <-chan struct{}) {
	t.Helper()
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:            src,
		Status:            sink,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, done)
}

// The defect: the fallback Nacked the job back to the queue and left the status
// row saying running.
func TestJobStatusStuck_NackFallbackLeavesNoRunningRow(t *testing.T) {
	src := &plainSource{
		job:     jobs.Job{ID: "j1", Type: "unknown.type", MaxRetry: 3},
		settled: make(chan struct{}),
	}
	sink := &recordingSink{}
	runUnhandled(t, src, sink, src.settled)

	// Give the detached terminal write a moment to land after Nack returns.
	waitForStatus(t, func() bool { return sink.final() != jobs.StatusRunning })

	if got := sink.final(); got != jobs.StatusFailed {
		t.Fatalf("status row ended at %q, want %q — the job is back in the queue, not executing", got, jobs.StatusFailed)
	}
}

// Nack dead-letters a job whose budget is spent, so the status row has to say
// so rather than reporting a retry that will never come.
func TestJobStatusStuck_NackFallbackRecordsDeathWhenBudgetSpent(t *testing.T) {
	src := &plainSource{
		// Delivery makes Attempts 1, which equals MaxRetry: Nack will dead-letter.
		job:     jobs.Job{ID: "j1", Type: "unknown.type", MaxRetry: 1},
		settled: make(chan struct{}),
	}
	sink := &recordingSink{}
	runUnhandled(t, src, sink, src.settled)

	waitForStatus(t, func() bool { return sink.final() != jobs.StatusRunning })

	if got := sink.final(); got != jobs.StatusDead {
		t.Fatalf("status row ended at %q, want %q — Nack dead-letters a job with no budget left", got, jobs.StatusDead)
	}
}

// Ordering: the queue is the source of truth. A requeue that failed means the
// queue still holds the job, so running is the honest status and the row must
// not claim otherwise.
func TestJobStatusStuck_FailedRequeueKeepsRunning(t *testing.T) {
	src := &failingRequeuer{plainSource: plainSource{
		job:     jobs.Job{ID: "j1", Type: "unknown.type", MaxRetry: 3},
		settled: make(chan struct{}),
	}}
	sink := &recordingSink{}
	runUnhandled(t, src, sink, src.settled)

	time.Sleep(50 * time.Millisecond) // let any stray transition land

	if got := sink.final(); got != jobs.StatusRunning {
		t.Fatalf("status row ended at %q after the requeue failed, want %q — the queue still holds this job", got, jobs.StatusRunning)
	}
}

// Same rule for the fallback: a failed Nack leaves the job held.
func TestJobStatusStuck_FailedNackKeepsRunning(t *testing.T) {
	src := &plainSource{
		job:     jobs.Job{ID: "j1", Type: "unknown.type", MaxRetry: 3},
		nackErr: errors.New("queue unreachable"),
		settled: make(chan struct{}),
	}
	sink := &recordingSink{}
	runUnhandled(t, src, sink, src.settled)

	time.Sleep(50 * time.Millisecond)

	if got := sink.final(); got != jobs.StatusRunning {
		t.Fatalf("status row ended at %q after the Nack failed, want %q", got, jobs.StatusRunning)
	}
}

// Over-reach guard: the Requeuer path, fixed under JB-4, still reports enqueued
// — a requeue does not spend the budget, so it must not read as failed.
func TestJobStatusStuck_RequeuePathStillReportsEnqueued(t *testing.T) {
	src := &requeueSource{
		ready:   []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}},
		settled: make(chan struct{}),
	}
	sink := &recordingSink{}
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:               src,
		Status:               sink,
		Concurrency:          1,
		EmptyQueueBackoff:    5 * time.Millisecond,
		MaxUnhandledRequeues: 2,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, src.settled)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var sawEnqueued bool
	for _, s := range sink.got {
		if s == jobs.StatusEnqueued {
			sawEnqueued = true
		}
		if s == jobs.StatusFailed {
			t.Fatalf("the requeue path reported %q; a requeue does not spend the retry budget", s)
		}
	}
	if !sawEnqueued {
		t.Fatal("the requeue path never moved the status row off running")
	}
}

// waitForStatus polls cond for up to a second so a detached terminal write has
// time to land without pinning the test to a fixed sleep. It returns quietly on
// timeout so the caller's own assertion reports the actual status.
func waitForStatus(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
