package events_test

// Audit EV-11: inproc.Bus.Publish spawned one goroutine per matching subscriber
// per event, and each of those blocked on the subscription's semaphore before
// doing anything. The semaphore bounds how many handlers *run* at once; it does
// not bound how many goroutines are queued behind it. A handler slower than the
// publish rate therefore accumulates goroutines without limit, each pinning its
// own copy of the event, until the process runs out of memory.
//
// Publish also had no way to say no: it always returned nil, so a caller could
// not tell that the bus was falling behind.
//
//	go test ./tests/events/... -run TestInprocBacklog

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
)

// blockingHandler parks until released, standing in for a handler slower than
// the publish rate.
type blockingHandler struct {
	release chan struct{}
	mu      sync.Mutex
	started int
}

func newBlockingHandler() *blockingHandler {
	return &blockingHandler{release: make(chan struct{})}
}

func (h *blockingHandler) handle(ctx context.Context, _ events.Event) error {
	h.mu.Lock()
	h.started++
	h.mu.Unlock()
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return nil
}

func (h *blockingHandler) unblock() { close(h.release) }

func (h *blockingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

// TestInprocBacklog_IsBounded is the EV-11 regression. Publishing far more
// events than the bus can process must not grow the goroutine count without
// limit.
func TestInprocBacklog_IsBounded(t *testing.T) {
	bus := inproc.New(inproc.Options{QueueSize: 16, DrainTimeout: 200 * time.Millisecond})
	h := newBlockingHandler()
	t.Cleanup(func() { h.unblock(); bus.Close() })

	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Handler:     h.handle,
		Concurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}

	base := runtime.NumGoroutine()

	// Far more than the queue can hold, with a handler that never completes.
	const published = 5000
	for i := range published {
		_ = bus.Publish(context.Background(),
			events.Event{ID: string(rune('a' + i%26)), Type: "x", Time: time.Now()})
	}

	// Let anything that was going to spawn, spawn.
	time.Sleep(100 * time.Millisecond)
	grew := runtime.NumGoroutine() - base

	// A bounded bus holds workers plus whatever is briefly in flight — tens, not
	// thousands. The old shape grew one goroutine per published event.
	if grew > 200 {
		t.Errorf("goroutine count grew by %d after %d publishes into a stalled bus: "+
			"the semaphore bounds execution but not the queue behind it, so a slow "+
			"handler accumulates goroutines until the process dies", grew, published)
	}
}

// A full queue must be reported, not silently absorbed. Returning nil while
// dropping the event is how a backlog becomes invisible.
func TestInprocBacklog_FullQueueIsReported(t *testing.T) {
	bus := inproc.New(inproc.Options{QueueSize: 4, DrainTimeout: 200 * time.Millisecond})
	h := newBlockingHandler()
	t.Cleanup(func() { h.unblock(); bus.Close() })

	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Handler:     h.handle,
		Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var lastErr error
	for range 200 {
		if err := bus.Publish(context.Background(),
			events.Event{ID: "flood", Type: "x", Time: time.Now()}); err != nil {
			lastErr = err
			break
		}
	}

	if lastErr == nil {
		t.Fatal("Publish never reported a full queue; the caller cannot tell the bus is " +
			"falling behind, which is how a backlog becomes invisible")
	}
	if !errors.Is(lastErr, inproc.ErrQueueFull) {
		t.Errorf("error = %v, want inproc.ErrQueueFull so callers can distinguish "+
			"backpressure from a delivery failure", lastErr)
	}
}

// Anti-vacuity: the ordinary path must still work. A bus that refused
// everything, or dropped events, would pass both tests above.
func TestInprocBacklog_NormalDeliveryUnaffected(t *testing.T) {
	bus := inproc.New()
	t.Cleanup(func() { bus.Close() })

	var mu sync.Mutex
	var got []string
	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"order.*"},
		Handler: func(_ context.Context, e events.Event) error {
			mu.Lock()
			got = append(got, e.ID)
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	for i := range 50 {
		if err := bus.Publish(context.Background(), events.Event{
			ID: string(rune('A' + i%26)), Type: "order.created", Time: time.Now(),
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// Non-matching events must still be ignored rather than queued.
	if err := bus.Publish(context.Background(),
		events.Event{ID: "nope", Type: "invoice.created", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 50 {
		t.Errorf("delivered %d event(s), want 50", len(got))
	}
	for _, id := range got {
		if id == "nope" {
			t.Error("a non-matching event was delivered")
		}
	}
}

// Concurrency must still cap how many handlers run at once — that is what the
// worker pool replaces the semaphore with, and losing it would let a bounded
// queue feed an unbounded number of concurrent handlers.
func TestInprocBacklog_ConcurrencyStillCapsParallelism(t *testing.T) {
	bus := inproc.New(inproc.Options{QueueSize: 64})
	t.Cleanup(func() { bus.Close() })

	var mu sync.Mutex
	var running, peak int
	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: 3,
		Handler: func(context.Context, events.Event) error {
			mu.Lock()
			running++
			if running > peak {
				peak = running
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	for range 30 {
		_ = bus.Publish(context.Background(), events.Event{ID: "c", Type: "x", Time: time.Now()})
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > 3 {
		t.Errorf("peak concurrent handlers = %d, want at most 3", peak)
	}
	if peak == 0 {
		t.Error("no handler ever ran")
	}
}

// Cancelling a subscription with events still queued must discount them.
//
// Each queued event was counted at Publish, so if a retired subscription's
// backlog is abandoned without releasing it, the bus's in-flight counter never
// reaches zero: Close then blocks for its whole DrainTimeout and reports an
// incomplete drain, for work nobody was ever going to do. Nothing else in the
// suite reaches that branch — removing the release passes every other test.
func TestInprocBacklog_CancelReleasesQueuedEvents(t *testing.T) {
	bus := inproc.New(inproc.Options{QueueSize: 16, DrainTimeout: 2 * time.Second})
	t.Cleanup(func() { bus.Close() })

	cancel, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: 1,
		Handler: func(context.Context, events.Event) error {
			time.Sleep(30 * time.Millisecond)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fill the queue well beyond what the single worker can drain immediately.
	for range 12 {
		if err := bus.Publish(context.Background(),
			events.Event{ID: "q", Type: "x", Time: time.Now()}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	cancel()

	start := time.Now()
	if err := bus.Close(); err != nil {
		t.Errorf("Close reported %v: the queued events of a cancelled subscription were "+
			"never discounted, so the drain waited on work that will never run", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v: it waited on abandoned queue entries rather than "+
			"releasing them", elapsed)
	}
}
