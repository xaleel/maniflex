package realtime_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Visibility evaluation (RT-9) ──────────────────────────────────────────────
//
// deliverWS ran applyVisibility inside the per-subID loop, so a client
// subscribed to one event type through several overlapping patterns invoked the
// (user-supplied) Visibility hook once per matching subscription. The hook's
// inputs are only the principal and the event — never the subID — so that work
// is redundant. It is now evaluated once per (client, event), while still
// delivering to every matching subscription.

// countingVisibility returns a hook that records how many times it runs and
// answers with the given verdict.
func countingVisibility(calls *atomic.Int64, deliver bool) realtime.VisibilityFunc {
	return func(_ *realtime.Principal, _ events.Event) (bool, *events.Event) {
		calls.Add(1)
		return deliver, nil
	}
}

func TestHub_VisibilityEvaluatedOncePerClient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Visibility: countingVisibility(&calls, true)})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// Three subscriptions that all match user.created — the redundant case.
	c.subscribe("user.*")
	c.subscribe("*")
	c.subscribe("user.created")

	publish(t, bus, "user.created", "user/1")

	// All three subscriptions still receive the event.
	for i := range 3 {
		if _, ok := c.recvTimeout(2 * time.Second); !ok {
			t.Fatalf("subscription %d did not receive the event: delivery to a matching sub was lost", i+1)
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("Visibility ran %d times for one client and one event; want 1 "+
			"(it does not depend on the subscription, so per-subID evaluation is wasted work)", n)
	}
}

// TestHub_VisibilitySuppressAppliesToEverySub is the over-reach guard: hoisting
// the hook must not change its meaning. A suppressing verdict still suppresses
// delivery to all of the client's matching subscriptions, and still runs once.
func TestHub_VisibilitySuppressAppliesToEverySub(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Visibility: countingVisibility(&calls, false)})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("user.*")
	c.subscribe("*")

	publish(t, bus, "user.created", "user/1")

	if _, ok := c.recvTimeout(300 * time.Millisecond); ok {
		t.Error("a suppressing Visibility verdict still delivered the event")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Visibility ran %d times; want 1", n)
	}
}

// TestHub_VisibilityNotCalledWithoutMatch pins the preserved semantics: a client
// with no matching subscription must not invoke the hook at all — the hoist
// keeps the original emptiness guard.
func TestHub_VisibilityNotCalledWithoutMatch(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Visibility: countingVisibility(&calls, true)})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("post.*") // does not match user.created

	publish(t, bus, "user.created", "user/1")

	if _, ok := c.recvTimeout(300 * time.Millisecond); ok {
		t.Error("received an event that matched no subscription")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Visibility ran %d times for an unmatched event; want 0", n)
	}
}
