package nats

// Audit EV-15: the durable consumer name was fmt.Sprintf("%s-%s", group,
// sanitise(subject)), and sanitise is many-to-one — it maps "." to "_", ">" to
// "all" and "*" to "any". So "invoice.>" and "invoice.all" both rendered as
// "invoice_all", and so did "a.b" and "a_b".
//
// A durable's filter subject is fixed when the consumer is created, so the
// collision is not silent: nats.go's processConsInfo refuses the second
// subscription with ErrSubjectMismatch. The bug is a legitimate configuration
// being rejected, not events going missing — worth stating plainly, because the
// audit reads as though delivery were affected.
//
// Found next to it, and larger: Subscription.Group is documented as "the
// consumer-group name for Kafka, JetStream, and Redis XREADGROUP", but this
// adapter called js.Subscribe with a Durable and no deliver group. A durable
// with no deliver group accepts exactly one bound subscription, so a second
// replica was refused with "consumer is already bound to a subscription" —
// Group provided no competing-consumer semantics at all here, while doing so on
// both other adapters.
//
// No NATS server is needed for any of this: the naming is a pure function, and
// the one JetStream call Subscribe makes goes through the jsOps seam.
//
//	go test ./events/nats/...

import (
	"context"
	"strings"
	"sync"
	"testing"

	natsclient "github.com/nats-io/nats.go"

	"github.com/xaleel/maniflex/events"
)

// ── naming ────────────────────────────────────────────────────────────────────

// The exact pairs that collided. Each is a plausible subject, not a contrivance:
// an event type ending in ".all" beside a wildcard over the same namespace is an
// ordinary thing to subscribe to.
func TestDurableName_PreviouslyCollidingPairsAreDistinct(t *testing.T) {
	pairs := [][2]string{
		{"*", "all"},
		{"invoice.*", "invoice.all"},
		{"order.*", "order.any"},
		{"a.b", "a_b"},
	}

	for _, p := range pairs {
		s0, s1 := patternToSubject(p[0]), patternToSubject(p[1])
		d0, d1 := durableName("g", s0), durableName("g", s1)

		if s0 == s1 {
			t.Fatalf("patterns %q and %q map to the same subject %q; the test pair is wrong", p[0], p[1], s0)
		}
		if d0 == d1 {
			t.Errorf("patterns %q and %q both produce durable %q: "+
				"the second subscription is refused with ErrSubjectMismatch", p[0], p[1], d0)
		}
	}
}

// The group half sanitises just as lossily as the subject half, so it has to be
// covered by the hash too.
func TestDurableName_CollidingGroupsAreDistinct(t *testing.T) {
	if a, b := durableName("a.b", "s"), durableName("a_b", "s"); a == b {
		t.Errorf("groups %q and %q both produce durable %q", "a.b", "a_b", a)
	}
}

// The separator must not be forgeable: without one, ("a", "b.c") and ("a.b",
// "c") hash the same bytes.
func TestDurableName_GroupAndSubjectCannotBeShifted(t *testing.T) {
	if a, b := durableName("a", "b.c"), durableName("a.b", "c"); a == b {
		t.Errorf("(%q,%q) and (%q,%q) both produce durable %q", "a", "b.c", "a.b", "c", a)
	}
}

// Same inputs must give the same name on every process and every restart, or a
// redeploy silently orphans its own durable and replays from the stream default.
func TestDurableName_IsStable(t *testing.T) {
	first := durableName("billing", "invoice.>")
	for range 100 {
		if got := durableName("billing", "invoice.>"); got != first {
			t.Fatalf("durable name is not deterministic: %q then %q", first, got)
		}
	}
}

// NATS rejects ".", "*" and ">" in durable and queue-group names, and the old
// sanitise replaced only those three — leaving any other invalid byte to be
// refused by the server at Subscribe.
func TestDurableName_ContainsOnlyLegalCharacters(t *testing.T) {
	subjects := []string{
		"invoice.>", "invoice.*", "a.b.c", "weird subject", "tab\there",
		"sl/ash", "back\\slash", "dollar$", "unicode-café", ">",
	}
	for _, s := range subjects {
		got := durableName("grp", s)
		for _, r := range got {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || r == '_' || r == '-'
			if !ok {
				t.Errorf("durableName(%q) = %q contains %q, which NATS rejects", s, got, r)
				break
			}
		}
	}
}

// The readable half exists for whoever runs `nats consumer ls`. If it were
// dropped the fix would still be correct and the names useless.
func TestDurableName_KeepsAReadablePrefix(t *testing.T) {
	got := durableName("billing", "invoice.>")
	if !strings.HasPrefix(got, "billing-invoice_all-") {
		t.Errorf("durable %q lost its readable prefix", got)
	}
}

// A pathological subject must not produce an unbounded name, and truncating the
// readable half must not cost uniqueness — the hash is appended after the cut.
func TestDurableName_LongSubjectsStayBoundedAndUnique(t *testing.T) {
	long1 := strings.Repeat("averylongtoken.", 40) + "one"
	long2 := strings.Repeat("averylongtoken.", 40) + "two"

	d1, d2 := durableName("g", long1), durableName("g", long2)
	if len(d1) > maxReadableLen+16 {
		t.Errorf("durable name is %d chars: %q", len(d1), d1)
	}
	if d1 == d2 {
		t.Errorf("two long subjects truncated to the same durable %q", d1)
	}
}

// ── wiring ────────────────────────────────────────────────────────────────────

type recordedSub struct {
	subject string
	queue   string
	durable string
}

type fakeSubscription struct{ unsubscribed bool }

func (f *fakeSubscription) Unsubscribe() error { f.unsubscribed = true; return nil }

type fakeOps struct {
	mu   sync.Mutex
	got  []recordedSub
	subs []*fakeSubscription
	err  error
}

func (f *fakeOps) QueueSubscribe(subject, queue, durable string, _ natsclient.MsgHandler) (jsSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.got = append(f.got, recordedSub{subject: subject, queue: queue, durable: durable})
	s := &fakeSubscription{}
	f.subs = append(f.subs, s)
	return s, nil
}

func (f *fakeOps) recorded() []recordedSub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedSub(nil), f.got...)
}

func busWithOps(ops jsOps) *Bus { return &Bus{ops: ops, stream: "events"} }

func subscribeWith(t *testing.T, ops jsOps, sub events.Subscription) events.Cancel {
	t.Helper()
	cancel, err := busWithOps(ops).Subscribe(context.Background(), sub)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(cancel)
	return cancel
}

func noopHandler(context.Context, events.Event) error { return nil }

// The Group fix: without a queue group the durable accepts one bound
// subscription, so replica two cannot subscribe at all.
func TestSubscribe_UsesGroupAsQueueGroup(t *testing.T) {
	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group: "billing", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	})

	got := ops.recorded()
	if len(got) != 1 {
		t.Fatalf("%d subscriptions, want 1", len(got))
	}
	if got[0].queue == "" {
		t.Error("no queue group: a second replica is refused with " +
			"\"consumer is already bound to a subscription\"")
	}
	if got[0].queue != "billing" {
		t.Errorf("queue group = %q, want %q", got[0].queue, "billing")
	}
}

// The queue group is subject to the same NATS naming rules as the durable.
func TestSubscribe_QueueGroupIsSanitised(t *testing.T) {
	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group: "team.billing", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	})

	if q := ops.recorded()[0].queue; strings.ContainsAny(q, ".*>") {
		t.Errorf("queue group %q contains a character NATS rejects", q)
	}
}

// Each pattern gets its own durable; two patterns sharing one would be the
// original bug in a new place.
func TestSubscribe_EachPatternGetsADistinctDurable(t *testing.T) {
	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group:    "billing",
		Patterns: []string{"invoice.*", "invoice.all", "order.created"},
		Handler:  noopHandler,
	})

	got := ops.recorded()
	if len(got) != 3 {
		t.Fatalf("%d subscriptions, want 3", len(got))
	}

	seen := map[string]string{}
	for _, r := range got {
		if prev, dup := seen[r.durable]; dup {
			t.Errorf("subjects %q and %q share durable %q", prev, r.subject, r.durable)
		}
		seen[r.durable] = r.subject
	}
}

// All replicas of the same group must compute identical durable names, or they
// each create their own consumer and every event is handled once per replica
// instead of once per group.
func TestSubscribe_SameGroupAndPatternGiveTheSameDurable(t *testing.T) {
	sub := events.Subscription{
		Group: "billing", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	}
	a, b := &fakeOps{}, &fakeOps{}
	subscribeWith(t, a, sub)
	subscribeWith(t, b, sub)

	if a.recorded()[0].durable != b.recorded()[0].durable {
		t.Errorf("two replicas computed different durables: %q and %q",
			a.recorded()[0].durable, b.recorded()[0].durable)
	}
}

// Distinct groups must not share a consumer, or two independent subscribers
// silently split one stream between them.
func TestSubscribe_DistinctGroupsGetDistinctDurables(t *testing.T) {
	a, b := &fakeOps{}, &fakeOps{}
	subscribeWith(t, a, events.Subscription{
		Group: "billing", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	})
	subscribeWith(t, b, events.Subscription{
		Group: "audit", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	})

	if a.recorded()[0].durable == b.recorded()[0].durable {
		t.Errorf("groups billing and audit share durable %q", a.recorded()[0].durable)
	}
}

// The collision test above catches a bad scheme by its symptom. This pins the
// wiring directly, so a call site that stops using durableName fails as
// "Subscribe is not using durableName" rather than as a mystery collision.
func TestSubscribe_DurableComesFromDurableName(t *testing.T) {
	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group: "billing", Patterns: []string{"invoice.*"}, Handler: noopHandler,
	})

	want := durableName("billing", "invoice.>")
	if got := ops.recorded()[0].durable; got != want {
		t.Errorf("durable = %q, want %q: Subscribe is not using durableName", got, want)
	}
}

// Anti-vacuity: the subject filter must still be the translated pattern. A fix
// that got the durable right and the subject wrong would pass everything above.
func TestSubscribe_SubjectFilterIsUnchanged(t *testing.T) {
	ops := &fakeOps{}
	subscribeWith(t, ops, events.Subscription{
		Group:    "billing",
		Patterns: []string{"invoice.*", "*", "order.created"},
		Handler:  noopHandler,
	})

	want := []string{"invoice.>", ">", "order.created"}
	got := ops.recorded()
	for i, w := range want {
		if got[i].subject != w {
			t.Errorf("subject[%d] = %q, want %q", i, got[i].subject, w)
		}
	}
}

// Cancel must release every subscription it opened, not just the last.
func TestSubscribe_CancelUnsubscribesAll(t *testing.T) {
	ops := &fakeOps{}
	cancel := subscribeWith(t, ops, events.Subscription{
		Group:    "billing",
		Patterns: []string{"invoice.*", "order.*"},
		Handler:  noopHandler,
	})
	cancel()

	ops.mu.Lock()
	defer ops.mu.Unlock()
	for i, s := range ops.subs {
		if !s.unsubscribed {
			t.Errorf("subscription %d was left open after Cancel", i)
		}
	}
}
