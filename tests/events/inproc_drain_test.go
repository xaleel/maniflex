package events_test

// Audit EV-6 (High), inproc half: Bus.Publish spawned one untracked goroutine
// per matching subscriber, each running DeliverWithRetry on context.Background(),
// and Bus.Close() was a no-op returning nil.
//
// So nothing waited for in-flight handlers and nothing could cancel them: a
// handler mid-retry at shutdown died with the process, and the ctx-cancellation
// path DeliverWithRetry implements could never fire, because Background is never
// cancelled.
//
//	go test ./tests/events/... -run TestInprocDrain

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
)

// slowHandler takes a measurable moment, and records whether it ran to
// completion or was cut short.
type slowHandler struct {
	delay time.Duration

	mu        sync.Mutex
	started   int
	finished  int
	sawCancel bool
}

func (h *slowHandler) handle(ctx context.Context, _ events.Event) error {
	h.mu.Lock()
	h.started++
	h.mu.Unlock()

	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		h.mu.Lock()
		h.sawCancel = true
		h.mu.Unlock()
		return ctx.Err()
	}

	h.mu.Lock()
	h.finished++
	h.mu.Unlock()
	return nil
}

func (h *slowHandler) counts() (started, finished int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started, h.finished
}

func (h *slowHandler) cancelled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sawCancel
}

// waitStarted blocks until the handler has begun, so the shutdown under test is
// not racing the delivery goroutine's start.
func (h *slowHandler) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := h.counts(); s > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("precondition: the handler never started")
}

// TestInprocDrain_CloseWaitsForInFlightHandlers is the EV-6 regression. Close
// must not return while a delivery is still running.
func TestInprocDrain_CloseWaitsForInFlightHandlers(t *testing.T) {
	bus := inproc.New()
	h := &slowHandler{delay: 150 * time.Millisecond}

	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  h.handle,
	}); err != nil {
		t.Fatal(err)
	}

	if err := bus.Publish(context.Background(),
		events.Event{ID: "drain-1", Type: "x", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.waitStarted(t)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	started, finished := h.counts()
	if finished != started {
		t.Errorf("%d handler(s) started but only %d finished: Close returned while a "+
			"delivery was still in flight, so the process can exit mid-handler",
			started, finished)
	}
}

// The second half: the delivery ran on context.Background(), so nothing could
// ask it to stop. A handler that outlives the drain budget must be signalled.
func TestInprocDrain_HandlerSeesCancellation(t *testing.T) {
	bus := inproc.New(inproc.Options{DrainTimeout: 200 * time.Millisecond})
	h := &slowHandler{delay: 10 * time.Second} // far beyond the drain budget

	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  h.handle,
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(),
		events.Event{ID: "drain-2", Type: "x", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.waitStarted(t)

	err := bus.Close()
	if err == nil {
		t.Error("Close reported a clean drain while a handler was still running; " +
			"a caller cannot tell a completed shutdown from an abandoned one")
	}
	if !h.cancelled() {
		t.Error("the handler never observed a cancellation signal: it is running on a context " +
			"nothing can cancel, so Close can only outlive it, never ask it to stop")
	}
}

// Anti-vacuity: Close must not become a blanket "cancel everything immediately".
// A handler that finishes within the budget has to finish normally, uncancelled.
func TestInprocDrain_FastHandlerCompletesUncancelled(t *testing.T) {
	bus := inproc.New(inproc.Options{DrainTimeout: 5 * time.Second})
	h := &slowHandler{delay: 10 * time.Millisecond}

	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  h.handle,
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(),
		events.Event{ID: "drain-3", Type: "x", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := bus.Close(); err != nil {
		t.Errorf("Close: %v — a delivery well inside the budget must drain cleanly", err)
	}
	if _, finished := h.counts(); finished != 1 {
		t.Errorf("handler finished %d time(s), want 1", finished)
	}
	if h.cancelled() {
		t.Error("the handler was cancelled despite finishing inside the drain budget")
	}
}

// Publishing after Close must not start work nobody will wait for. Returning an
// error is what tells a caller their event went nowhere — silently accepting it
// would be a fresh way to lose events at shutdown, which is the whole finding.
func TestInprocDrain_PublishAfterCloseIsRefused(t *testing.T) {
	bus := inproc.New()
	h := &slowHandler{delay: time.Millisecond}
	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  h.handle,
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}

	err := bus.Publish(context.Background(),
		events.Event{ID: "after-close", Type: "x", Time: time.Now()})
	if err == nil {
		t.Error("Publish after Close returned nil; the caller believes the event was accepted")
	}
	if !errors.Is(err, inproc.ErrBusClosed) {
		t.Errorf("error = %v, want inproc.ErrBusClosed so callers can distinguish shutdown "+
			"from a real delivery failure", err)
	}
	if started, _ := h.counts(); started != 0 {
		t.Errorf("handler started %d time(s) after Close", started)
	}
}
