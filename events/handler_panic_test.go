package events

// A panicking Subscription.Handler used to unwind out of the broker's delivery
// goroutine, where nothing recovered it, so the Go runtime killed the binary:
// one bad event type took down every other subscription and the HTTP server
// with it. Jobs already had this right — jobs.Worker.runHandler recovers and
// nacks — and events did not (audit H2).
//
// The tests that assert containment fail by *crashing the test binary* before
// the fix, which is the failure mode being fixed.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// A panic must be contained and turned into a failed attempt, so the retry the
// subscription already asks for still happens. A handler that panics once and
// then succeeds has delivered its event.
func TestDeliverWithRetry_HandlerPanicIsRetriedLikeAnError(t *testing.T) {
	captureLogs(t)
	calls := 0
	sub := Subscription{
		Handler: func(context.Context, Event) error {
			calls++
			if calls == 1 {
				panic("boom on the first attempt")
			}
			return nil
		},
		MaxRetry: 2, // Backoff nil → no sleeping
	}
	pub := &recordingPublisher{}

	DeliverWithRetry(context.Background(), pub, sub, Event{ID: "p1", Type: "widget.created"})

	if calls != 2 {
		t.Fatalf("handler called %d times, want 2 — a panicking attempt must be retried "+
			"exactly like one that returned an error", calls)
	}
	if len(pub.published) != 0 {
		t.Fatalf("event was dead-lettered despite the retry succeeding: %+v", pub.published)
	}
}

// A handler that panics every time must exhaust its retries and dead-letter,
// rather than taking the process down on the first one.
func TestDeliverWithRetry_PersistentHandlerPanicDeadLetters(t *testing.T) {
	captureLogs(t)
	calls := 0
	sub := Subscription{
		Handler:  func(context.Context, Event) error { calls++; panic("boom every time") },
		MaxRetry: 1, // 2 attempts
		DLQ:      "widget.created.dlq",
	}
	pub := &recordingPublisher{}

	DeliverWithRetry(context.Background(), pub, sub, Event{ID: "p2", Type: "widget.created"})

	if calls != 2 {
		t.Fatalf("handler called %d times, want 2 (MaxRetry=1)", calls)
	}
	if len(pub.published) != 1 || pub.published[0].Type != "widget.created.dlq" {
		t.Fatalf("a persistently panicking handler must dead-letter its event, got %+v",
			pub.published)
	}
}

// Containment without a report would be a silent swallow: the panic is the bug,
// and the stack is the only thing that says where it is.
func TestDeliverWithRetry_HandlerPanicIsLoggedWithStack(t *testing.T) {
	logs := captureLogs(t)
	sub := Subscription{
		Handler:  func(context.Context, Event) error { panic("boom worth logging") },
		MaxRetry: 0, // single attempt, so exactly one panic to find
	}

	DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "p3", Type: "widget.created"})

	var found *capturedLog
	for i, l := range *logs {
		if l.level == slog.LevelError && strings.Contains(l.msg, "panic") {
			found = &(*logs)[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no ERROR record reporting the handler panic; logs: %+v", *logs)
	}
	if got, _ := found.attrs["panic"].(string); !strings.Contains(got, "boom worth logging") {
		t.Errorf("panic attr = %q, want the panic value", got)
	}
	if got, _ := found.attrs["stack"].(string); !strings.Contains(got, "TestDeliverWithRetry_HandlerPanicIsLoggedWithStack") {
		t.Errorf("stack attr does not reach the panicking call site:\n%s", got)
	}
	if got, _ := found.attrs["id"].(string); got != "p3" {
		t.Errorf("id attr = %q, want the event id so the panic ties to an event", got)
	}
}

// Subscription.OnPanic is the programmatic signal, so an application can count
// or alert on handler panics without parsing logs.
func TestSubscriptionOnPanic_ReceivesEventPanicAndStack(t *testing.T) {
	captureLogs(t)
	var (
		mu    sync.Mutex
		calls int
		gotEv Event
		gotRe any
		gotSt []byte
	)
	sub := Subscription{
		Handler:  func(context.Context, Event) error { panic("boom the hook should see") },
		MaxRetry: 0,
		OnPanic: func(e Event, recovered any, stack []byte) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			gotEv, gotRe, gotSt = e, recovered, stack
		},
	}

	DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "p4", Type: "widget.created"})

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("OnPanic called %d times, want exactly 1", calls)
	}
	if gotEv.ID != "p4" {
		t.Errorf("OnPanic got event %q, want the event that panicked", gotEv.ID)
	}
	if gotRe != "boom the hook should see" {
		t.Errorf("OnPanic got recovered value %#v, want the panic value", gotRe)
	}
	if !strings.Contains(string(gotSt), "TestSubscriptionOnPanic_ReceivesEventPanicAndStack") {
		t.Errorf("OnPanic got a stack that does not reach the call site:\n%s", gotSt)
	}
}

// One panic per attempt, so a hook counting them sees every one rather than
// only the first.
func TestSubscriptionOnPanic_FiresOncePerPanickingAttempt(t *testing.T) {
	captureLogs(t)
	var mu sync.Mutex
	calls := 0
	sub := Subscription{
		Handler:  func(context.Context, Event) error { panic("boom every time") },
		MaxRetry: 2, // 3 attempts
		OnPanic: func(Event, any, []byte) {
			mu.Lock()
			defer mu.Unlock()
			calls++
		},
	}

	DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "p5", Type: "widget.created"})

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("OnPanic called %d times, want 3 (one per attempt)", calls)
	}
}
