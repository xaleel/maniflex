package events_test

// Audit C1: Bus.Publish counts an event into the drain WaitGroup and then sends
// it to a subscription's queue, holding nothing across the two steps. A Cancel
// that retires that subscription's workers in the gap leaves the event in a
// queue nobody drains and the count it was given is never balanced, so Close
// burns its whole DrainTimeout and reports ErrDrainIncomplete over one event
// that was never going to be delivered.
//
// The same gap swallows a Close. Publish clears the closed check, Close then
// finds the counter at zero and returns nil, and only afterwards does Publish
// add its count. The caller is told the shutdown was clean while an event the
// bus accepted was dropped — which is precisely the failure Close exists to
// report, reported as success.
//
//	go test ./tests/events/... -run TestInprocRace

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
)

const (
	// raceType is the only event type these tests publish.
	raceType = "race.hit"

	// raceFillers subscriptions, each carrying racePatterns patterns that never
	// match, sit between the sentinel and the subscription under test. Publish
	// walks every subscription in one pass, so the fillers stretch the gap
	// between "Publish has started" and "Publish reaches the last subscription"
	// from nanoseconds into milliseconds. The race is real without them, but
	// too narrow to reproduce on demand — and a regression test that only
	// sometimes reproduces is not a regression test.
	raceFillers  = 2000
	racePatterns = 40

	// raceDrain is short so a leaked count costs a second rather than the 30s a
	// real deployment would spend, but not so short that a loaded machine can
	// fail the fixed code: the drain has to outlast one instant handler under
	// contention from ~2000 idle goroutines, and a budget tuned to the happy
	// path is how the export and query-timeout tests became flaky.
	raceDrain = time.Second
)

// newRaceBus returns a bus whose subscriptions are [sentinel, fillers...] and a
// channel closed once the sentinel has been delivered — that is, once Publish is
// committed to the walk with milliseconds of filler still ahead of it. Anything
// the caller subscribes afterwards is therefore reached last, long after the
// signal fires, which is what makes the window reachable on purpose.
func newRaceBus(t *testing.T) (*inproc.Bus, <-chan struct{}) {
	t.Helper()
	bus := inproc.New(inproc.Options{QueueSize: 8, DrainTimeout: raceDrain})
	t.Cleanup(func() { bus.Close() })

	reached := make(chan struct{})
	var once sync.Once
	if _, err := bus.Subscribe(context.Background(), events.Subscription{
		Patterns: []string{raceType},
		Handler: func(context.Context, events.Event) error {
			once.Do(func() { close(reached) })
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	pats := make([]string, racePatterns)
	for i := range pats {
		pats[i] = fmt.Sprintf("filler-%02d.*.never", i)
	}
	for range raceFillers {
		if _, err := bus.Subscribe(context.Background(), events.Subscription{
			Patterns: pats,
			Handler:  func(context.Context, events.Event) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	return bus, reached
}

func raceEvent() events.Event {
	return events.Event{ID: "race-1", Type: raceType, Time: time.Now()}
}

// counting returns a subscription whose handler tallies its deliveries.
func counting(n *atomic.Int64) events.Subscription {
	return events.Subscription{
		Patterns: []string{raceType},
		Handler: func(context.Context, events.Event) error {
			n.Add(1)
			return nil
		},
	}
}

// C1 proper: a Cancel that runs to completion while Publish is mid-walk must not
// strand the event Publish is about to enqueue. The existing backlog test covers
// events already queued when Cancel is called; this is the one that arrives
// after the workers are gone, which nothing releases.
func TestInprocRace_CancelDuringPublishDoesNotStrandTheEvent(t *testing.T) {
	bus, reached := newRaceBus(t)

	// The subscription under test, reached last, cancelled mid-walk.
	var target atomic.Int64
	cancel, err := bus.Subscribe(context.Background(), counting(&target))
	if err != nil {
		t.Fatal(err)
	}

	// A control behind it that is never cancelled. One retiring subscriber must
	// not cost the rest their copy, and its delivery is what proves Publish
	// really walked the whole list instead of stopping at the casualty.
	var control atomic.Int64
	if _, err := bus.Subscribe(context.Background(), counting(&control)); err != nil {
		t.Fatal(err)
	}

	cancelled := make(chan struct{})
	go func() {
		<-reached
		cancel()
		close(cancelled)
	}()

	if err := bus.Publish(context.Background(), raceEvent()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	<-cancelled

	start := time.Now()
	cerr := bus.Close()
	elapsed := time.Since(start)

	if cerr != nil {
		t.Errorf("Close: %v — the event landed in the queue of a subscription whose "+
			"workers had already retired, so the count Publish took for it is never "+
			"balanced and the drain waits out its whole budget on a delivery nobody "+
			"will ever perform", cerr)
	}
	if elapsed >= raceDrain {
		t.Errorf("Close took %v of its %v budget: it blocked on a stranded event rather "+
			"than draining, which in production is a 30s hang on every shutdown that "+
			"races an unsubscribe", elapsed.Round(time.Millisecond), raceDrain)
	}
	if control.Load() != 1 {
		t.Errorf("the subscription behind the cancelled one received %d copies, want 1: "+
			"one subscriber retiring must not deny the rest their copy", control.Load())
	}
	t.Logf("cancelled subscription received %d (either outcome is legitimate)", target.Load())
}

// The same window with Close as the racer. Publish clears the closed check,
// Close observes a zero counter and reports a clean drain, and only then does
// the event get enqueued. Whatever Publish accepts must be delivered before
// Close can call the shutdown clean.
func TestInprocRace_CloseNeverReportsACleanDrainForAnUndeliveredEvent(t *testing.T) {
	bus, reached := newRaceBus(t)

	var delivered atomic.Int64
	if _, err := bus.Subscribe(context.Background(), counting(&delivered)); err != nil {
		t.Fatal(err)
	}

	var cerr error
	closed := make(chan struct{})
	go func() {
		<-reached
		cerr = bus.Close()
		close(closed)
	}()

	perr := bus.Publish(context.Background(), raceEvent())
	<-closed

	// Publish must hold the bus's read lock across its whole walk and Close must
	// need the write lock, so a Publish already past the sentinel cannot be
	// overtaken. This is therefore the accepted branch, not the refused one — if
	// it ever reports ErrBusClosed the window has moved and the assertion below
	// has stopped meaning anything.
	if perr != nil {
		t.Fatalf("Publish: %v — a Publish already walking the subscription list was "+
			"overtaken by Close, so this test no longer exercises the race it exists for",
			perr)
	}
	if n := delivered.Load(); n != 1 {
		t.Errorf("Publish accepted the event and Close returned %v, but the handler ran "+
			"%d times: the bus reported a clean shutdown over an event it had already "+
			"told the caller it would deliver, so nothing is left to log or alert on",
			cerr, n)
	}
	if cerr != nil {
		t.Errorf("Close: %v — the handler is instant and well inside the budget", cerr)
	}
}
