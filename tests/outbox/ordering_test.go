package outbox_test

// Audit EV-10 (outbox half): a row that fails delivery gets next_attempt_at
// pushed out by markError, but later rows keep shipping on the next poll. So for
// one aggregate, event 2 can reach the broker before the retried event 1 —
// a stale overwrite, where the older state is applied last.
//
// The relayer processes a claimed batch in created_at order, so ordering holds
// while everything succeeds. It is precisely the retry that reorders, which is
// why this needs a failing first delivery to show at all.
//
//	go test ./tests/outbox/... -run TestOrdering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/outbox"
)

// failFirstN fails the first n publishes of a given event ID, then accepts, and
// records the order in which events were actually delivered.
type failFirstN struct {
	mu       sync.Mutex
	failFor  map[string]int
	order    []string
	failures int
}

func newFailFirstN(failFor map[string]int) *failFirstN {
	return &failFirstN{failFor: failFor}
}

func (p *failFirstN) Publish(_ context.Context, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.failFor[e.ID]; ok && n > 0 {
		p.failFor[e.ID] = n - 1
		p.failures++
		return errors.New("transient broker error")
	}
	p.order = append(p.order, e.ID)
	return nil
}

func (p *failFirstN) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *failFirstN) Close() error { return nil }

func (p *failFirstN) delivered() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

// aggregateEvent builds an event for one aggregate, so Subject is the key that
// ties them together — the same value Kafka already uses as its partition key.
func aggregateEvent(id, subject string) events.Event {
	e := makeEvent(id)
	e.Subject = subject
	return e
}

// publishAll inserts events in order, with distinct created_at values so the
// relayer's ORDER BY created_at is deterministic rather than relying on
// insertion order within one clock tick.
func publishAll(t *testing.T, bus *outbox.Bus, evs ...events.Event) {
	t.Helper()
	for _, e := range evs {
		if err := bus.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestOrdering_RetriedEventDoesNotFallBehindItsSuccessor is the EV-10
// regression. Two events for one aggregate; the first fails once. With ordering
// on, the second must not overtake it.
func TestOrdering_RetriedEventDoesNotFallBehindItsSuccessor(t *testing.T) {
	db := openTestDB(t)
	pub := newFailFirstN(map[string]int{"ord-1": 1})
	bus := outbox.Wrap(pub, db, "sqlite")

	publishAll(t, bus,
		aggregateEvent("ord-1", "invoice/abc"),
		aggregateEvent("ord-2", "invoice/abc"),
	)

	runRelay(t, bus, outbox.RelayOptions{
		MaxAttempts:  5,
		OrderedByKey: true,
		DLQType:      "test.created.dlq",
	}, 2500*time.Millisecond)

	got := pub.delivered()
	if len(got) != 2 {
		t.Fatalf("delivered %v, want both events", got)
	}
	if got[0] != "ord-1" || got[1] != "ord-2" {
		t.Errorf("delivery order = %v, want [ord-1 ord-2]: the retried first event was "+
			"overtaken by its successor, so the older state is applied last", got)
	}
}

// Anti-vacuity, and the trade-off made explicit: ordering is per key. A stalled
// aggregate must not hold up an unrelated one, or one poison record stops the
// whole relay.
func TestOrdering_OneStalledKeyDoesNotBlockAnother(t *testing.T) {
	db := openTestDB(t)
	// "stuck" never succeeds within the window; "other" should sail past it.
	pub := newFailFirstN(map[string]int{"stuck-1": 99})
	bus := outbox.Wrap(pub, db, "sqlite")

	publishAll(t, bus,
		aggregateEvent("stuck-1", "invoice/aaa"),
		aggregateEvent("other-1", "invoice/bbb"),
	)

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 50, OrderedByKey: true},
		600*time.Millisecond)

	got := pub.delivered()
	if len(got) != 1 || got[0] != "other-1" {
		t.Errorf("delivered %v, want [other-1]: ordering is per aggregate, so a stalled "+
			"key must not block an unrelated one", got)
	}
}

// Off by default: ordering costs head-of-line blocking, so an app that has not
// asked for it keeps shipping later events while an earlier one backs off.
func TestOrdering_DisabledByDefaultKeepsShipping(t *testing.T) {
	db := openTestDB(t)
	pub := newFailFirstN(map[string]int{"def-1": 99})
	bus := outbox.Wrap(pub, db, "sqlite")

	publishAll(t, bus,
		aggregateEvent("def-1", "invoice/xyz"),
		aggregateEvent("def-2", "invoice/xyz"),
	)

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 50}, 600*time.Millisecond)

	got := pub.delivered()
	if len(got) != 1 || got[0] != "def-2" {
		t.Errorf("delivered %v, want [def-2]: without OrderedByKey the relay must not "+
			"introduce head-of-line blocking", got)
	}
}

// A row with no Subject has no aggregate to be ordered against. Those must keep
// flowing rather than serialising into one implicit "" bucket, which would turn
// ordering on for every event that simply did not set a Subject.
func TestOrdering_RowsWithoutAKeyAreNotSerialised(t *testing.T) {
	db := openTestDB(t)
	pub := newFailFirstN(map[string]int{"nokey-1": 99})
	bus := outbox.Wrap(pub, db, "sqlite")

	// makeEvent leaves Subject empty.
	publishAll(t, bus, makeEvent("nokey-1"), makeEvent("nokey-2"))

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 50, OrderedByKey: true},
		600*time.Millisecond)

	got := pub.delivered()
	if len(got) != 1 || got[0] != "nokey-2" {
		t.Errorf("delivered %v, want [nokey-2]: keyless rows share no aggregate and must "+
			"not be held behind each other", got)
	}
}

// The ordering_key column is added by ALTER TABLE for tables created before it
// existed, so the upgrade path — not just a fresh CREATE — has to work, and
// Migrate has to stay idempotent because it runs on every start.
func TestOrdering_MigrationAddsColumnToExistingTable(t *testing.T) {
	db := openRawDB(t)

	// The pre-ordering_key schema, as an existing deployment has it.
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE event_outbox (
			id              TEXT PRIMARY KEY,
			type            TEXT NOT NULL,
			payload         TEXT NOT NULL,
			created_at      TIMESTAMP NOT NULL,
			shipped_at      TIMESTAMP,
			next_attempt_at TIMESTAMP,
			lease_until     TIMESTAMP,
			lease_id        TEXT,
			attempts        INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT
		)`); err != nil {
		t.Fatal(err)
	}

	if err := outbox.Migrate(context.Background(), db, "sqlite"); err != nil {
		t.Fatalf("migrating an existing table: %v", err)
	}
	// Twice: Migrate runs on every start, and the second ADD COLUMN must be
	// recognised as already-done rather than failing the boot.
	if err := outbox.Migrate(context.Background(), db, "sqlite"); err != nil {
		t.Fatalf("second Migrate must be a no-op, got: %v", err)
	}

	// The column has to be usable, not merely present.
	bus := outbox.Wrap(&nopPublisher{}, db, "sqlite")
	if err := bus.Publish(context.Background(), aggregateEvent("mig-1", "invoice/mig")); err != nil {
		t.Fatalf("publish after migration: %v", err)
	}
	var key string
	if err := db.QueryRowContext(context.Background(),
		`SELECT ordering_key FROM event_outbox WHERE id = ?`, "mig-1").Scan(&key); err != nil {
		t.Fatalf("read ordering_key: %v", err)
	}
	if key != "invoice/mig" {
		t.Errorf("ordering_key = %q, want the event Subject %q", key, "invoice/mig")
	}
}
