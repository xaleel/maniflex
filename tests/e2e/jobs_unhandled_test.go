package e2e

// Audit JB-4 / JB-9: when a worker dequeues a job whose type it has no handler
// for, it requeues the job so another worker can claim it. That requeue was
// unbounded and abused the retry budget, and each adapter diverged:
//
//   - jobs/redis re-enqueued unconditionally, so a type NO worker handled
//     bounced between stream and delayed-set forever (JB-4);
//   - jobs/sql and jobs/inproc requeued through Nack, spending the retry budget
//     a real handler would later need, and eventually killing the job (JB-9).
//
// The fix is at the worker: it requeues through the Requeuer interface, which
// re-persists the job without spending an attempt, counts the requeues in a
// header, and dead-letters once the count reaches MaxUnhandledRequeues — so a
// misconfigured type surfaces instead of storming, and a real handler keeps its
// full budget. Adapters without Requeuer keep the legacy Nack fallback.
//
//	go test ./e2e/ -run TestUnhandled

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// requeueSource is a fake Source+Requeuer that models a single job cycling
// through the queue: Dequeue hands it out (incrementing Attempts as a real
// adapter does), Requeue puts it back with whatever the worker stored.
type requeueSource struct {
	mu sync.Mutex

	ready []jobs.Job // jobs claimable now (attempts is the stored count)

	requeued     []jobs.Job // one entry per Requeue call, as passed
	deaded       []string
	nacked       []string
	seenAttempts []int // Attempts observed by the worker at each delivery

	settled chan struct{} // closed once the job is deaded or nacked
	once    sync.Once
}

func (s *requeueSource) Dequeue(_ context.Context, n int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ready) == 0 {
		return nil, nil
	}
	j := s.ready[0]
	s.ready = s.ready[1:]
	j.Attempts++ // an adapter sets the current attempt count on delivery
	s.seenAttempts = append(s.seenAttempts, j.Attempts)
	return []jobs.Job{j}, nil
}

func (s *requeueSource) Ack(context.Context, string) error { return nil }

func (s *requeueSource) Nack(_ context.Context, id string, _ error, _ time.Duration) error {
	s.mu.Lock()
	s.nacked = append(s.nacked, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *requeueSource) Dead(_ context.Context, id string, _ error) error {
	s.mu.Lock()
	s.deaded = append(s.deaded, id)
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *requeueSource) Requeue(_ context.Context, j jobs.Job, _ time.Duration) error {
	s.mu.Lock()
	s.requeued = append(s.requeued, j)
	s.ready = append(s.ready, j) // becomes claimable again
	s.mu.Unlock()
	return nil
}

func (s *requeueSource) signal() { s.once.Do(func() { close(s.settled) }) }

// sourceNoRequeuer is a Source that does NOT implement Requeuer, to exercise
// the legacy Nack fallback. It hands the job once.
type sourceNoRequeuer struct {
	mu      sync.Mutex
	handed  bool
	nacked  []string
	deaded  []string
	nackSig chan struct{}
}

func (s *sourceNoRequeuer) Dequeue(context.Context, int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handed {
		return nil, nil
	}
	s.handed = true
	return []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}}, nil
}
func (s *sourceNoRequeuer) Ack(context.Context, string) error { return nil }
func (s *sourceNoRequeuer) Nack(_ context.Context, id string, _ error, _ time.Duration) error {
	s.mu.Lock()
	s.nacked = append(s.nacked, id)
	s.mu.Unlock()
	select {
	case s.nackSig <- struct{}{}:
	default:
	}
	return nil
}
func (s *sourceNoRequeuer) Dead(_ context.Context, id string, _ error) error {
	s.mu.Lock()
	s.deaded = append(s.deaded, id)
	s.mu.Unlock()
	return nil
}

func runWorkerUntil(t *testing.T, cfg jobs.WorkerConfig, done <-chan struct{}) {
	t.Helper()
	w, err := jobs.NewWorker(cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { _ = w.Run(ctx); close(finished) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-finished
		t.Fatal("worker did not settle the job within 3s")
	}
	cancel()
	<-finished
}

// The JB-4 regression: a job no worker handles must stop being requeued and be
// dead-lettered after the bound, not bounce forever.
func TestUnhandled_RequeuesAreBoundedThenDeadLettered(t *testing.T) {
	src := &requeueSource{
		ready:   []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}},
		settled: make(chan struct{}),
	}
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:               src,
		Concurrency:          1,
		EmptyQueueBackoff:    5 * time.Millisecond,
		MaxUnhandledRequeues: 3,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, src.settled)

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.requeued) != 3 {
		t.Errorf("requeued %d times, want exactly 3 (MaxUnhandledRequeues) before dead-lettering", len(src.requeued))
	}
	if len(src.deaded) != 1 || src.deaded[0] != "j1" {
		t.Errorf("dead-lettered %v, want [j1] once — a type no worker handles must surface", src.deaded)
	}
}

// The counter that bounds the storm must actually be carried on the job and
// grow monotonically, or the bound could never be reached.
func TestUnhandled_CounterGrowsInHeader(t *testing.T) {
	src := &requeueSource{
		ready:   []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}},
		settled: make(chan struct{}),
	}
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:               src,
		Concurrency:          1,
		EmptyQueueBackoff:    5 * time.Millisecond,
		MaxUnhandledRequeues: 3,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, src.settled)

	src.mu.Lock()
	defer src.mu.Unlock()
	for i, j := range src.requeued {
		got := j.Headers[jobs.HeaderUnhandledRequeues]
		if want := strconv.Itoa(i + 1); got != want {
			t.Errorf("requeue %d carried counter %q, want %q", i, got, want)
		}
	}
}

// The JB-9 half: an unhandled requeue must not spend the retry budget. The
// worker undoes the delivery's attempt increment before requeuing, so the
// effective attempt count never climbs across the bounces.
func TestUnhandled_DoesNotConsumeRetryBudget(t *testing.T) {
	src := &requeueSource{
		ready:   []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}},
		settled: make(chan struct{}),
	}
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:               src,
		Concurrency:          1,
		EmptyQueueBackoff:    5 * time.Millisecond,
		MaxUnhandledRequeues: 5,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, src.settled)

	src.mu.Lock()
	defer src.mu.Unlock()
	// Every delivery must see Attempts == 1: the increment from this delivery,
	// with all prior unhandled deliveries undone. If the budget were consumed it
	// would climb 1, 2, 3, ... and the job would exhaust MaxRetry=3 and die of
	// "retries" it never actually used.
	for i, a := range src.seenAttempts {
		if a != 1 {
			t.Errorf("delivery %d saw Attempts=%d, want 1 — the retry budget is being consumed by requeues", i, a)
		}
	}
	// And it was dead-lettered by the unhandled bound (5), not by MaxRetry (3).
	if len(src.deaded) != 1 {
		t.Fatalf("dead-lettered %d times, want 1", len(src.deaded))
	}
	if len(src.requeued) != 5 {
		t.Errorf("requeued %d times, want 5 — a budget-driven death would have stopped at 3", len(src.requeued))
	}
}

// A Source without Requeuer keeps the legacy behaviour: a single Nack, no
// dead-letter, no bounding. Third-party adapters must not silently change.
func TestUnhandled_FallsBackToNackWithoutRequeuer(t *testing.T) {
	src := &sourceNoRequeuer{nackSig: make(chan struct{}, 1)}
	runWorkerUntil(t, jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 5 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	}, src.nackSig)

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.nacked) != 1 || src.nacked[0] != "j1" {
		t.Errorf("nacked %v, want [j1] via the legacy fallback", src.nacked)
	}
	if len(src.deaded) != 0 {
		t.Errorf("dead-lettered %v via the fallback path; it should Nack, not Dead", src.deaded)
	}
}
