package inproc_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maniflex/events"
	"maniflex/events/inproc"
)

// noBackoff avoids real sleeps in tests that exercise retry paths.
var noBackoff = func(int) time.Duration { return 0 }

func makeEvent(id, typ string) events.Event {
	return events.Event{ID: id, Type: typ, Source: "test", Time: time.Now().UTC()}
}

// collect subscribes and returns a channel that receives delivered event types.
// Unsubscribes via the returned cancel when the test ends.
func collect(t *testing.T, b *inproc.Bus, patterns []string) (chan string, events.Cancel) {
	t.Helper()
	ch := make(chan string, 20)
	cancel, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns: patterns,
		Handler: func(_ context.Context, e events.Event) error {
			ch <- e.Type
			return nil
		},
		MaxRetry: 0,
		Backoff:  noBackoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ch, cancel
}

// drain waits up to timeout for n items on ch, then returns them.
func drain(ch <-chan string, n int, timeout time.Duration) []string {
	deadline := time.After(timeout)
	var got []string
	for len(got) < n {
		select {
		case v := <-ch:
			got = append(got, v)
		case <-deadline:
			return got
		}
	}
	return got
}

// ── pattern matching ──────────────────────────────────────────────────────────

func TestInproc_PatternMatching_Glob(t *testing.T) {
	b := inproc.New()
	ch, cancel := collect(t, b, []string{"invoice.*"})
	t.Cleanup(cancel)

	b.Publish(context.Background(), makeEvent("1", "invoice.created"))
	b.Publish(context.Background(), makeEvent("2", "order.created"))
	b.Publish(context.Background(), makeEvent("3", "invoice.updated"))

	got := drain(ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 deliveries for pattern invoice.*, got %d: %v", len(got), got)
	}
	for _, typ := range got {
		if typ != "invoice.created" && typ != "invoice.updated" {
			t.Errorf("unexpected event type %q delivered to invoice.* subscriber", typ)
		}
	}
}

func TestInproc_PatternMatching_Wildcard(t *testing.T) {
	b := inproc.New()
	ch, cancel := collect(t, b, []string{"*"})
	t.Cleanup(cancel)

	b.Publish(context.Background(), makeEvent("a", "foo.bar"))
	b.Publish(context.Background(), makeEvent("b", "baz.qux"))

	got := drain(ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 deliveries for *, got %d", len(got))
	}
}

func TestInproc_PatternMatching_ExactMatch(t *testing.T) {
	b := inproc.New()
	ch, cancel := collect(t, b, []string{"job.done"})
	t.Cleanup(cancel)

	b.Publish(context.Background(), makeEvent("1", "job.done"))
	b.Publish(context.Background(), makeEvent("2", "job.failed"))

	got := drain(ch, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 delivery for exact pattern job.done, got %d", len(got))
	}
}

func TestInproc_PatternMatching_MultiPattern(t *testing.T) {
	b := inproc.New()
	ch, cancel := collect(t, b, []string{"a.*", "b.*"})
	t.Cleanup(cancel)

	b.Publish(context.Background(), makeEvent("1", "a.x"))
	b.Publish(context.Background(), makeEvent("2", "b.y"))
	b.Publish(context.Background(), makeEvent("3", "c.z"))

	got := drain(ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 deliveries for [a.* b.*], got %d: %v", len(got), got)
	}
}

// ── cancel removes subscriber ─────────────────────────────────────────────────

func TestInproc_Cancel_StopsDelivery(t *testing.T) {
	b := inproc.New()
	ch, cancel := collect(t, b, []string{"*"})

	b.Publish(context.Background(), makeEvent("before", "x"))
	if got := drain(ch, 1, time.Second); len(got) != 1 {
		t.Fatal("expected delivery before cancel")
	}

	cancel() // unsubscribe

	b.Publish(context.Background(), makeEvent("after", "x"))
	if got := drain(ch, 1, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("expected no delivery after cancel, got %d", len(got))
	}
}

func TestInproc_Cancel_Idempotent(t *testing.T) {
	b := inproc.New()
	_, cancel := collect(t, b, []string{"*"})
	// Calling cancel multiple times must not panic.
	cancel()
	cancel()
	cancel()
}

// ── DLQ on exhausted retries ──────────────────────────────────────────────────

func TestInproc_DLQ_PublishedAfterExhaustedRetries(t *testing.T) {
	b := inproc.New()
	dlqReceived := make(chan events.Event, 1)

	// DLQ subscriber — registered first so it's ready when the DLQ event arrives.
	dlqCancel, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"dead.event"},
		Handler: func(_ context.Context, e events.Event) error {
			dlqReceived <- e
			return nil
		},
		MaxRetry: 0,
		Backoff:  noBackoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dlqCancel)

	// Primary subscriber: always fails.
	primaryCancel, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"invoice.created"},
		Handler:  func(_ context.Context, _ events.Event) error { return errors.New("always fail") },
		MaxRetry: 1,
		Backoff:  noBackoff,
		DLQ:      "dead.event",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(primaryCancel)

	b.Publish(context.Background(), makeEvent("orig-1", "invoice.created"))

	select {
	case dlq := <-dlqReceived:
		if dlq.Headers["original_type"] != "invoice.created" {
			t.Errorf("DLQ event missing original_type header: %v", dlq.Headers)
		}
		if dlq.Type != "dead.event" {
			t.Errorf("DLQ event type: got %q, want %q", dlq.Type, "dead.event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: DLQ event not received after exhausted retries")
	}
}

func TestInproc_DLQ_NotPublishedOnSuccess(t *testing.T) {
	b := inproc.New()
	dlqReceived := make(chan struct{}, 1)

	dlqCancel, _ := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"dead.event"},
		Handler: func(_ context.Context, _ events.Event) error {
			dlqReceived <- struct{}{}
			return nil
		},
		MaxRetry: 0,
	})
	t.Cleanup(dlqCancel)

	primaryCancel, _ := b.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"invoice.created"},
		Handler:  func(_ context.Context, _ events.Event) error { return nil }, // success
		MaxRetry: 0,
		DLQ:      "dead.event",
	})
	t.Cleanup(primaryCancel)

	b.Publish(context.Background(), makeEvent("ok-1", "invoice.created"))

	select {
	case <-dlqReceived:
		t.Fatal("DLQ event published even though handler succeeded")
	case <-time.After(200 * time.Millisecond):
		// correct: no DLQ event
	}
}

// ── concurrency limit ─────────────────────────────────────────────────────────

// TestInproc_ConcurrencyLimit is a RED test for E5.
//
// inproc spawns one goroutine per delivery regardless of sub.Concurrency, so
// Concurrency=1 is silently ignored — multiple handlers run in parallel.
//
// This test asserts that at most Concurrency handlers run at the same time.
// It currently FAILS because inproc does not implement a worker pool.
//
// Fix (E5): replace the per-message goroutine in inproc.Publish with a
// buffered-channel worker pool of size sub.Concurrency.
func TestInproc_ConcurrencyLimit(t *testing.T) {
	const concurrency = 1
	const numEvents = 5

	b := inproc.New()

	var (
		active    atomic.Int64
		maxActive atomic.Int64
		done      sync.WaitGroup
	)
	done.Add(numEvents)

	_, err := b.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: concurrency,
		MaxRetry:    0,
		Backoff:     noBackoff,
		Handler: func(_ context.Context, _ events.Event) error {
			cur := active.Add(1)
			// Track high-water mark.
			for {
				old := maxActive.Load()
				if cur <= old || maxActive.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond) // hold slot long enough for others to arrive
			active.Add(-1)
			done.Done()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := range numEvents {
		b.Publish(context.Background(), makeEvent(string(rune('a'+i)), "thing.happened"))
	}

	// Wait for all handlers to complete (with timeout).
	ch := make(chan struct{})
	go func() { done.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for all handlers")
	}

	if max := maxActive.Load(); max > concurrency {
		t.Fatalf("max concurrent handlers = %d; want <= %d (E5: inproc ignores sub.Concurrency)", max, concurrency)
	}
}
