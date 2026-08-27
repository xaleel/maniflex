package nats

// Audit C3: the JetStream dispatch callback spawned a goroutine per message and
// only then acquired the concurrency semaphore inside it. Subscription.Concurrency
// therefore bounded how many handlers *ran*, and nothing at all bounded how many
// goroutines waited behind them — each one holding a decoded event and its own
// stack — for as long as the handler stayed slower than the message rate.
//
// The correct order is the one events/redis and the inproc bus already use:
// take the slot first, and only spawn once you hold it. Doing that in the
// dispatch callback means the callback blocks, which is exactly the point —
// nats.go calls it serially from one goroutine per subscription, so a blocked
// callback is how an async subscriber tells the client to stop handing it work,
// and an unacked message is how the client tells the server the same thing.
//
// No NATS server is needed: the dispatch callback is captured through the jsOps
// seam and driven the way nats.go drives it, serially from one goroutine.
//
//	go test ./events/nats/... -run TestDispatch

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	natsclient "github.com/nats-io/nats.go"

	"github.com/xaleel/maniflex/events"
)

// dispatchMsg builds the message a JetStream push consumer would deliver. The
// Msg is unbound, so its Ack is a no-op error the adapter already ignores.
func dispatchMsg(t *testing.T, i int) *natsclient.Msg {
	t.Helper()
	payload, err := json.Marshal(events.Event{
		ID: string(rune('a' + i%26)), Type: "invoice.created", Time: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &natsclient.Msg{Subject: "invoice.created", Data: payload}
}

// parkedHandler reports each entry on started and then blocks until release is
// closed, standing in for a handler slower than the message rate.
func parkedHandler(started chan<- struct{}, release <-chan struct{}) events.Handler {
	return func(context.Context, events.Event) error {
		started <- struct{}{}
		<-release
		return nil
	}
}

// The finding itself: once Concurrency handlers are in flight, the dispatch
// callback must stop accepting messages instead of parking another goroutine
// behind the semaphore for each one.
func TestDispatch_StopsAcceptingOnceTheSlotsAreTaken(t *testing.T) {
	const (
		concurrency = 2
		messages    = 6
	)
	started := make(chan struct{}, messages)
	release := make(chan struct{})

	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group:       "billing",
		Patterns:    []string{"*"},
		Concurrency: concurrency,
		Handler:     parkedHandler(started, release),
	})
	cb := ops.onlyHandler(t)

	// Driven serially from one goroutine, which is how nats.go invokes an async
	// subscription's callback. Feeding it concurrently would test a client that
	// does not exist.
	var accepted atomic.Int64
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for i := range messages {
			cb(dispatchMsg(t, i))
			accepted.Add(1)
		}
	}()

	// Blocks until the pool is genuinely full, which also proves Concurrency
	// handlers run at once rather than the semaphore serialising them to one.
	for range concurrency {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the pool never filled: no handler is being reached at all")
		}
	}

	// Nothing can release a slot until the handlers are, so a correct dispatcher
	// is now blocked for good and this wait can only expire. It is headroom
	// rather than a threshold — a longer one would only make the test safer,
	// where the old code finishes all six in microseconds.
	select {
	case <-drained:
		t.Fatalf("the dispatch callback accepted all %d messages while only %d handlers "+
			"can run: every one past the limit is a goroutine parked on the semaphore "+
			"pinning its own event, and nothing bounds how many of those accumulate",
			messages, concurrency)
	case <-time.After(250 * time.Millisecond):
	}
	if n := accepted.Load(); n > concurrency {
		t.Errorf("%d messages accepted with %d slots: the callback is not the thing "+
			"waiting for a slot", n, concurrency)
	}

	// And it must be a gate, not a wedge: releasing the handlers has to let the
	// rest through. A dispatcher that blocks permanently would pass every
	// assertion above while delivering nothing.
	close(release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch callback never resumed after the handlers finished")
	}
	for range messages - concurrency {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d messages ever reached the handler", len(started), messages)
		}
	}
}

// Blocking the callback introduces a hazard the old code did not have: a
// dispatcher waiting for a slot that never frees. Cancel has to be able to
// release it, or unsubscribing during a stall wedges nats.go's delivery
// goroutine for that subscription until the process ends.
func TestDispatch_CancelReleasesAWaitingCallback(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)

	ops := &fakeOps{}
	cancel := subscribeWith(t, ops, events.Subscription{
		Group:       "billing",
		Patterns:    []string{"*"},
		Concurrency: 1,
		Handler:     parkedHandler(started, release),
	})
	cb := ops.onlyHandler(t)

	// Fill the only slot, then leave a second callback waiting on it.
	go cb(dispatchMsg(t, 0))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first handler never started")
	}

	returned := make(chan struct{})
	go func() {
		cb(dispatchMsg(t, 1))
		close(returned)
	}()

	cancel()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a dispatch callback waiting for a slot did not return when the " +
			"subscription was cancelled: nats.go delivers this subscription from one " +
			"goroutine, so it stays blocked for the life of the process")
	}
}
