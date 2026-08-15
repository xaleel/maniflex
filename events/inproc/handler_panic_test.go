package inproc

// The unit tests in events/ prove DeliverWithRetry contains a handler panic.
// This pins what that containment is worth at a broker: the worker goroutine
// that ran the panicking handler has to survive it, keep draining its queue,
// and keep its inflight accounting balanced — otherwise a panic that no longer
// kills the process would instead wedge Close on a delivery nobody will finish.
//
// It also covers the audit's headline claim directly: one bad subscription must
// not end the others (audit H2).

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
)

// zeroBackoff keeps the retries this bus performs from sleeping the test; the
// bus supplies a linear backoff when Subscription.Backoff is nil.
func zeroBackoff(int) time.Duration { return 0 }

// silenceLogs discards the default logger for the duration of the test. The
// deliveries here panic on purpose, and each one reports a full stack — correct
// in production, six stack traces of noise in a passing test run. Not
// parallel-safe: it swaps the global default.
func silenceLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// A handler that panics must not stop its own subscription from receiving the
// next event, must not stop a sibling subscription from receiving the same one,
// and must leave the bus able to drain.
func TestBus_PanickingHandlerDoesNotStopDelivery(t *testing.T) {
	silenceLogs(t)
	b := New(Options{DrainTimeout: 5 * time.Second})

	var mu sync.Mutex
	var sibling []string
	panics := 0

	done := make(chan struct{}, 8)

	cancelPanic, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"widget.*"},
		MaxRetry: 1,
		Backoff:  zeroBackoff,
		Handler: func(_ context.Context, e events.Event) error {
			mu.Lock()
			panics++
			mu.Unlock()
			panic("boom from " + e.ID)
		},
	})
	if err != nil {
		t.Fatalf("subscribe panicking handler: %v", err)
	}
	defer cancelPanic()

	cancelSibling, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"widget.*"},
		MaxRetry: 1,
		Backoff:  zeroBackoff,
		Handler: func(_ context.Context, e events.Event) error {
			mu.Lock()
			sibling = append(sibling, e.ID)
			mu.Unlock()
			done <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("subscribe sibling handler: %v", err)
	}
	defer cancelSibling()

	for _, id := range []string{"e1", "e2", "e3"} {
		if err := b.Publish(context.Background(), events.Event{ID: id, Type: "widget.created"}); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	// The sibling is the observable proof the bus kept running past the panics.
	for range 3 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			mu.Lock()
			got := append([]string(nil), sibling...)
			mu.Unlock()
			t.Fatalf("sibling subscription stopped receiving after a sibling handler panicked; got %v", got)
		}
	}

	// A panicking delivery still has to be discounted, or Close waits out its
	// full budget on work that already finished.
	if err := b.Close(); err != nil {
		t.Fatalf("Close after panicking deliveries: %v — the inflight count was left "+
			"unbalanced by a delivery that panicked", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sibling) != 3 {
		t.Errorf("sibling handler saw %v, want all three events", sibling)
	}
	// MaxRetry=1 → two attempts per event, each panicking.
	if panics != 6 {
		t.Errorf("panicking handler ran %d times, want 6 (3 events × 2 attempts)", panics)
	}
}
