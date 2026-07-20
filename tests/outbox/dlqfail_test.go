package outbox_test

// Audit EV-9: when a row exhausts its attempts the relayer routes it to the DLQ
// and then marks it shipped — unconditionally, whether or not the dead-letter
// was actually published.
//
// The DLQ rides the same downstream broker that just failed MaxAttempts times,
// so "the DLQ publish also failed" is not an edge case: it is the expected
// shape of a broker outage. The row is then marked shipped, swept, and gone.
// The outbox exists precisely so that a broker outage cannot lose an event, and
// this is the one path where it does.
//
// The logging half of this landed with 11D.10 — the failure is reported. What
// was still open is that the row is discarded anyway.
//
//	go test ./tests/outbox/... -run TestDLQFail

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/outbox"
)

// deadBroker fails everything, which is what a broker outage looks like from the
// relayer: the original publish fails, and so does the dead-letter.
type deadBroker struct {
	mu       sync.Mutex
	attempts int
}

func (p *deadBroker) Publish(_ context.Context, _ events.Event) error {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	return errors.New("broker unreachable")
}

func (p *deadBroker) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *deadBroker) Close() error { return nil }

func (p *deadBroker) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// TestDLQFail_RowSurvivesWhenDeadLetterAlsoFails is the EV-9 regression. With
// the whole broker down, the event must still be in the outbox afterwards.
func TestDLQFail_RowSurvivesWhenDeadLetterAlsoFails(t *testing.T) {
	db := openTestDB(t)
	broker := &deadBroker{}
	bus := outbox.Wrap(broker, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("ev-outage")); err != nil {
		t.Fatal(err)
	}

	// MaxAttempts 1 so the first failure exhausts it and reaches the DLQ path.
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1, DLQType: "test.created.dlq"},
		300*time.Millisecond)

	if broker.count() < 2 {
		t.Fatalf("precondition: broker saw %d publish(es), want at least 2 "+
			"(the delivery and the dead-letter)", broker.count())
	}

	shipped, _, lastErr := rowState(t, db, "ev-outage")
	if shipped {
		t.Errorf("row was marked shipped although the dead-letter was never published "+
			"(last error %q): the event is now unrecoverable, and the outbox exists "+
			"precisely so a broker outage cannot do that", lastErr)
	}
}

// The row must also stay *claimable*, not merely unshipped. Leaving attempts at
// MaxAttempts satisfies the assertion above while claimBatch's
// "attempts < MaxAttempts" filter never selects it again — unshipped, unretried,
// and invisible, which is the EV-8 failure arriving by a different route.
func TestDLQFail_RowStaysClaimable(t *testing.T) {
	db := openTestDB(t)
	broker := &deadBroker{}
	bus := outbox.Wrap(broker, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("ev-claimable")); err != nil {
		t.Fatal(err)
	}
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1, DLQType: "test.created.dlq"},
		300*time.Millisecond)

	_, attempts, _ := rowState(t, db, "ev-claimable")
	if attempts >= 1 {
		t.Errorf("attempts = %d with MaxAttempts 1: claimBatch selects only rows with "+
			"attempts < MaxAttempts, so this row will never be picked up again", attempts)
	}
}

// Recovery is the point: once the broker comes back, the retained row must
// actually be delivered. Without this the fix is just a slower way to lose it.
func TestDLQFail_DeliversOnceBrokerRecovers(t *testing.T) {
	db := openTestDB(t)
	broker := &flakyBroker{failing: true}
	bus := outbox.Wrap(broker, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("ev-recover")); err != nil {
		t.Fatal(err)
	}

	// Outage: exhaust attempts and fail the dead-letter too.
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1, DLQType: "test.created.dlq"},
		300*time.Millisecond)
	if shipped, _, _ := rowState(t, db, "ev-recover"); shipped {
		t.Fatal("precondition: the row was discarded during the outage")
	}

	// Broker returns. The window must outlast the hold backoff, which is
	// relayBackoff(MaxAttempts) — 1s at MaxAttempts 1. Backing off before
	// retrying a broker that was just down is deliberate, so the test waits it
	// out rather than the code retrying tightly.
	broker.recover()
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1, DLQType: "test.created.dlq"},
		1500*time.Millisecond)

	if shipped, _, _ := rowState(t, db, "ev-recover"); !shipped {
		t.Error("the retained row was never delivered after the broker recovered; " +
			"holding a row nobody ever retries is not durability")
	}
	if got := broker.delivered(); len(got) != 1 || got[0].ID != "ev-recover" {
		t.Errorf("delivered %v, want exactly the original event", got)
	}
}

// Anti-vacuity: a working DLQ must still resolve the row. A fix that simply
// stopped marking rows shipped would pass everything above and turn every
// dead-lettered event into a permanent table entry.
func TestDLQFail_SuccessfulDeadLetterStillShipsRow(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("ev-dlq-ok")); err != nil {
		t.Fatal(err)
	}
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1, DLQType: dlqType},
		300*time.Millisecond)

	if len(pub.dead()) != 1 {
		t.Fatalf("precondition: %d dead-letters, want 1", len(pub.dead()))
	}
	if shipped, _, _ := rowState(t, db, "ev-dlq-ok"); !shipped {
		t.Error("a successfully dead-lettered row was not marked shipped; it would be " +
			"retried forever and never swept")
	}
}

// With no DLQ configured there is nowhere to route to, and RelayOptions.DLQType
// documents that the row is dropped. That is a deliberate opt-out, so it must
// not be swept up by this fix.
func TestDLQFail_NoDLQConfiguredStillDropsRow(t *testing.T) {
	db := openTestDB(t)
	bus := outbox.Wrap(&deadBroker{}, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("ev-nodlq")); err != nil {
		t.Fatal(err)
	}
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 1}, 300*time.Millisecond)

	if shipped, _, _ := rowState(t, db, "ev-nodlq"); !shipped {
		t.Error("row retained with no DLQ configured; RelayOptions.DLQType documents " +
			"that dead-lettering is disabled and the row dropped")
	}
}

// flakyBroker fails until recover() is called, then accepts and records.
type flakyBroker struct {
	mu      sync.Mutex
	failing bool
	got     []events.Event
}

func (p *flakyBroker) Publish(_ context.Context, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failing {
		return errors.New("broker unreachable")
	}
	p.got = append(p.got, e)
	return nil
}

func (p *flakyBroker) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *flakyBroker) Close() error { return nil }

func (p *flakyBroker) recover() {
	p.mu.Lock()
	p.failing = false
	p.mu.Unlock()
}

func (p *flakyBroker) delivered() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.got...)
}
