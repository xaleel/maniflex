package outbox_test

// Audit EV-8: an outbox row whose payload will not decode is a poison pill.
//
// processRow calls markError on an unmarshal failure, which increments attempts
// and schedules a retry — but the payload cannot become decodable, so every
// retry fails identically. Once attempts reaches MaxAttempts, claimBatch's
// "attempts < MaxAttempts" filter stops selecting the row entirely, and sweep
// only deletes rows with shipped_at set. So the row is never delivered, never
// dead-lettered, never cleaned up: it sits in the table forever, and every such
// row accumulates.
//
// The publish-failure path already handles exhaustion correctly (DLQ then mark
// shipped). The decode path simply never got the same treatment.
//
//	go test ./tests/outbox/... -run TestPoison

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/outbox"
)

// insertRaw writes an outbox row directly, bypassing Publish's marshalling —
// the only way to produce a payload that will not decode. Real ones arrive by
// schema drift, a truncated write, or a payload written by an older version.
func insertRaw(t *testing.T, db *sql.DB, id, eventType, payload string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO event_outbox (id, type, payload, created_at, attempts) VALUES (?, ?, ?, ?, 0)`,
		id, eventType, payload, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert raw row: %v", err)
	}
}

// rowState reads back what the relayer did to a row.
func rowState(t *testing.T, db *sql.DB, id string) (shipped bool, attempts int, lastErr string) {
	t.Helper()
	var shippedAt sql.NullTime
	var le sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT shipped_at, attempts, last_error FROM event_outbox WHERE id = ?`, id).
		Scan(&shippedAt, &attempts, &le)
	if err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return shippedAt.Valid, attempts, le.String
}

func countRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event_outbox`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// runRelay drives the relayer for a short window.
func runRelay(t *testing.T, bus *outbox.Bus, opts outbox.RelayOptions, d time.Duration) {
	t.Helper()
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	bus.Relay(opts).Start(ctx) //nolint:errcheck
}

// TestPoison_UndecodableRowIsResolvedNotStuck is the EV-8 regression. The row
// must reach a terminal state rather than sitting unshipped forever.
func TestPoison_UndecodableRowIsResolvedNotStuck(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	insertRaw(t, db, "poison-1", "test.created", "{not json at all")

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 3, DLQType: dlqType}, 300*time.Millisecond)

	shipped, attempts, lastErr := rowState(t, db, "poison-1")
	if !shipped {
		t.Errorf("row is still unshipped after %d attempt(s) (last error %q): an undecodable "+
			"payload can never decode, so it will be retried to exhaustion and then skipped "+
			"by claimBatch forever — stuck in the table with nothing to clean it up",
			attempts, lastErr)
	}
}

// It must also be dead-lettered, not silently discarded. Marking it shipped
// without emitting anything would fix the stuck row by losing the event, which
// is a worse trade.
func TestPoison_UndecodableRowIsDeadLettered(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	insertRaw(t, db, "poison-2", "test.created", "{truncated")

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 3, DLQType: dlqType}, 300*time.Millisecond)

	dead := pub.dead()
	if len(dead) != 1 {
		t.Fatalf("dead-lettered %d event(s), want 1: the payload is unreadable, so the DLQ "+
			"is the only place it can still be inspected", len(dead))
	}
	d := dead[0]
	if d.Type != dlqType {
		t.Errorf("Type = %q, want %q", d.Type, dlqType)
	}
	if d.Headers["original_id"] != "poison-2" {
		t.Errorf("original_id = %q, want the outbox row id %q — it is the only handle on a "+
			"row whose payload could not be parsed", d.Headers["original_id"], "poison-2")
	}
	if d.Headers["original_type"] != "test.created" {
		t.Errorf("original_type = %q, want the row's type column %q",
			d.Headers["original_type"], "test.created")
	}
	// The raw bytes must travel with it: they are the only remaining evidence.
	if len(d.Data) == 0 {
		t.Error("dead-letter carries no payload; the undecodable bytes are lost forever")
	}
}

// A decode failure is terminal — the bytes cannot become valid — so burning
// MaxAttempts retries on it only delays the DLQ and holds a row in the claim
// rotation. It must resolve on the first pass.
func TestPoison_DecodeFailureIsTerminalNotRetried(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	insertRaw(t, db, "poison-3", "test.created", "nonsense")

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 10, DLQType: dlqType}, 300*time.Millisecond)

	shipped, attempts, _ := rowState(t, db, "poison-3")
	if !shipped {
		t.Fatal("row not resolved")
	}
	if attempts > 1 {
		t.Errorf("attempts = %d, want at most 1: retrying a payload that cannot parse is "+
			"pure delay, and each retry re-enters the claim batch", attempts)
	}
}

// With no DLQ configured there is nowhere to send it, but the row must still
// resolve — and the loss has to be logged rather than the row vanishing
// silently. This pins that the fix does not depend on a DLQ being configured.
func TestPoison_ResolvesWithoutDLQConfigured(t *testing.T) {
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: "unused"}
	bus := outbox.Wrap(pub, db, "sqlite")

	insertRaw(t, db, "poison-4", "test.created", "{bad")

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 3}, 300*time.Millisecond)

	if shipped, _, _ := rowState(t, db, "poison-4"); !shipped {
		t.Error("row is stuck when no DLQ is configured; it accumulates forever")
	}
}

// The "unbounded dead-row growth" half of the finding: sweep deletes rows by
// "shipped_at IS NOT NULL AND shipped_at < cutoff", so a row that is never
// marked shipped is never eligible, whatever the retention. Asserting the
// column directly rather than running sweep, whose interval is
// max(PollInterval*60, 1m) and cannot be tuned down for a test.
func TestPoison_ResolvedRowBecomesSweepEligible(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	insertRaw(t, db, "poison-5", "test.created", "{bad")
	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 3, DLQType: dlqType}, 300*time.Millisecond)

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event_outbox WHERE id = ? AND shipped_at IS NOT NULL`,
		"poison-5").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the poison row does not match sweep's delete predicate, so nothing will " +
			"ever remove it and the table grows without bound")
	}
	if total := countRows(t, db); total != 1 {
		t.Fatalf("fixture drifted: %d rows, want 1", total)
	}
}

// Anti-vacuity: a well-formed event must still be delivered normally. A fix that
// dead-lettered everything, or marked rows shipped without publishing, would
// pass every assertion above.
func TestPoison_ValidEventStillDeliveredNormally(t *testing.T) {
	db := openTestDB(t)
	pub := &collectAll{}
	bus := outbox.Wrap(pub, db, "sqlite")

	if err := bus.Publish(context.Background(), makeEvent("good-1")); err != nil {
		t.Fatal(err)
	}
	insertRaw(t, db, "poison-6", "test.created", "{bad")

	runRelay(t, bus, outbox.RelayOptions{MaxAttempts: 3, DLQType: "test.created.dlq"},
		300*time.Millisecond)

	var deliveredGood bool
	for _, e := range pub.all() {
		if e.ID == "good-1" {
			deliveredGood = true
		}
	}
	if !deliveredGood {
		t.Error("the valid event was not delivered; a poison row must not stall the batch")
	}
	if shipped, _, _ := rowState(t, db, "good-1"); !shipped {
		t.Error("the valid row was not marked shipped")
	}
}

// collectAll accepts everything and records it.
type collectAll struct {
	mu  sync.Mutex
	got []events.Event
}

func (p *collectAll) Publish(_ context.Context, e events.Event) error {
	p.mu.Lock()
	p.got = append(p.got, e)
	p.mu.Unlock()
	return nil
}

func (p *collectAll) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *collectAll) Close() error { return nil }

func (p *collectAll) all() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.got...)
}
