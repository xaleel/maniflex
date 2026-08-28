package inproc

// Audit C4: Nack and Requeue each spawned a goroutine to sleep out the retry
// delay and then pulse notifyCh, so a job type failing hot parked one sleeping
// goroutine per attempt for the length of its backoff.
//
// The reason they were pure waste is the part the audit does not reach: nothing
// ever received from notifyCh. Dequeue drains it non-blockingly and discards the
// value, and the Queue did not implement jobs.BlockingSource, so every one of
// those goroutines slept and then wrote to a channel with no reader. The Worker
// meanwhile fell back to polling, whose empty-queue backoff doubles from 1s to
// 30s while a queue is idle — so a retry scheduled 100ms out could sit for half
// a minute past its due time, which is exactly what the pulse was built to
// prevent and never could.
//
//	go test ./jobs/inproc/ -run 'TestNack_DoesNot|TestDequeueBlocking'

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// The finding itself: retries must not each cost a parked goroutine.
func TestNack_DoesNotSpawnAGoroutinePerRetry(t *testing.T) {
	const retries = 500
	ctx := context.Background()
	q := New()
	t.Cleanup(func() { q.Close() })

	ids := make([]string, retries)
	for i := range ids {
		ids[i] = mustEnqueue(t, q, jobs.Job{Type: "hot", MaxRetry: 100})
	}
	if _, err := q.Dequeue(ctx, retries); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	runtime.Gosched()
	before := runtime.NumGoroutine()

	for _, id := range ids {
		// An hour, so nothing spawned here can have exited again by the time we
		// count: what we are measuring is what a backoff parks, not what a
		// finished backoff leaves behind.
		if err := q.Nack(ctx, id, errors.New("boom"), time.Hour); err != nil {
			t.Fatalf("nack: %v", err)
		}
	}

	runtime.Gosched()
	if grew := runtime.NumGoroutine() - before; grew > retries/10 {
		t.Errorf("%d retries left %d extra goroutines alive: each sleeps out its own "+
			"backoff, so a job type that fails hot parks one goroutine per attempt "+
			"for as long as its delay lasts", retries, grew)
	}
}

// ── DequeueBlocking ───────────────────────────────────────────────────────────

// blockingDequeue runs DequeueBlocking and reports what came back and how long
// it took, so each test below asserts on the wait rather than inferring it.
func blockingDequeue(t *testing.T, ctx context.Context, q *Queue, max time.Duration) ([]jobs.Job, time.Duration) {
	t.Helper()
	start := time.Now()
	js, err := q.DequeueBlocking(ctx, 10, max)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DequeueBlocking: %v", err)
	}
	return js, elapsed
}

// The point of implementing BlockingSource: a delayed retry must be picked up
// when it comes due, not whenever the poll cycle next comes round. This is the
// wait the per-retry goroutines were spawned to perform, now done once by
// whoever is idle rather than once per rescheduled job.
func TestDequeueBlocking_WakesWhenADelayedJobComesDue(t *testing.T) {
	ctx := context.Background()
	q := New()
	t.Cleanup(func() { q.Close() })

	if _, err := q.EnqueueAt(ctx, jobs.Job{Type: "later"}, time.Now().Add(150*time.Millisecond)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 5s of budget against 150ms of delay: a consumer that cannot see the due
	// time waits the whole budget out, so the gap is the assertion and the
	// bound is headroom rather than a deadline.
	js, elapsed := blockingDequeue(t, ctx, q, 5*time.Second)
	if len(js) != 1 {
		t.Fatalf("got %d jobs after %v, want the one that came due", len(js), elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("waited %v for a job due in 150ms: the consumer is sitting out its whole "+
			"budget instead of waking when the job is ready", elapsed)
	}
}

// The other half of the wake-up: a job arriving while a consumer is parked must
// reach it without waiting for the budget. This is what notifyCh was always for.
func TestDequeueBlocking_WakesOnANewEnqueue(t *testing.T) {
	ctx := context.Background()
	q := New()
	t.Cleanup(func() { q.Close() })

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = q.Enqueue(ctx, jobs.Job{Type: "arrived"})
	}()

	js, elapsed := blockingDequeue(t, ctx, q, 5*time.Second)
	if len(js) != 1 {
		t.Fatalf("got %d jobs after %v, want the one that was enqueued", len(js), elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("waited %v for a job enqueued after 100ms: the arrival is not waking "+
			"the parked consumer", elapsed)
	}
}

// The contract jobs.BlockingSource states: empty slice, not an error, on timeout
// — and not before the budget is spent, or the Worker's backoff means nothing.
func TestDequeueBlocking_ReturnsEmptyAtTheDeadline(t *testing.T) {
	ctx := context.Background()
	q := New()
	t.Cleanup(func() { q.Close() })

	const max = 300 * time.Millisecond
	js, elapsed := blockingDequeue(t, ctx, q, max)
	if len(js) != 0 {
		t.Fatalf("got %d jobs from an empty queue", len(js))
	}
	if elapsed < max-50*time.Millisecond {
		t.Errorf("returned after %v of a %v budget: an early return puts the Worker "+
			"straight back into the poll-and-sleep this exists to avoid", elapsed, max)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %v, well past its %v budget", elapsed, max)
	}
}

// Shutdown must not have to wait out the budget.
func TestDequeueBlocking_ReturnsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := New()
	t.Cleanup(func() { q.Close() })

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	js, elapsed := blockingDequeue(t, ctx, q, 10*time.Second)
	if len(js) != 0 {
		t.Fatalf("got %d jobs", len(js))
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v to notice a cancelled context", elapsed)
	}
}

// A job can be due and still unclaimable, because a GroupKey it shares is
// running. Waking "when the earliest job comes due" must not mean waking
// instantly and forever on a due time that has already passed: that turns an
// idle consumer into a spin for the whole budget.
func TestDequeueBlocking_DoesNotSpinOnAJobItCannotClaim(t *testing.T) {
	ctx := context.Background()
	q := New()
	t.Cleanup(func() { q.Close() })

	// Claim one job for the group, so the second is due but held off.
	mustEnqueue(t, q, jobs.Job{Type: "a", GroupKey: "g"})
	if got, err := q.Dequeue(ctx, 10); err != nil || len(got) != 1 {
		t.Fatalf("precondition: Dequeue got %d jobs, err %v", len(got), err)
	}
	mustEnqueue(t, q, jobs.Job{Type: "b", GroupKey: "g"})

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	js, elapsed := blockingDequeue(t, ctx, q, 400*time.Millisecond)
	runtime.ReadMemStats(&after)

	if len(js) != 0 {
		t.Fatalf("got %d jobs while the group key was held", len(js))
	}
	if elapsed < 350*time.Millisecond {
		t.Errorf("returned after %v of a 400ms budget", elapsed)
	}
	// Each turn of the wait loop builds a timer, so the allocation count is the
	// iteration count. Measured: 9 allocations waiting properly, 480888 spinning
	// through the same 400ms. The bound sits three orders of magnitude above the
	// first and nearly two below the second, so neither a slow machine nor a
	// busy one moves it across.
	if allocs := after.Mallocs - before.Mallocs; allocs > 10_000 {
		t.Errorf("the wait allocated %d times in %v: it is spinning on a due time that "+
			"has already passed rather than waiting", allocs, elapsed)
	}
}
