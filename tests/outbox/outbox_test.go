package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/outbox"
	_ "modernc.org/sqlite"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// openRawDB opens an empty database without running Migrate, for tests that
// need to control the starting schema.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// A file in the test's temp dir, not ":memory:".
	//
	// Every connection to ":memory:" opens its OWN empty database, so a pooled
	// *sql.DB spreads one test across several of them — the relayer writes on
	// one connection and an assertion reads on another, finding "no such table:
	// event_outbox". It surfaced as a flake (measured 3 runs in 8), which is
	// worse than a failure because it reads as someone else's problem.
	//
	// The obvious fix, a shared-cache DSN like db/sqlite.Open uses, was tried and
	// still flaked: a shared-cache in-memory database lives only while at least
	// one connection is open, and database/sql may retire and reopen a pooled
	// connection at any moment — which silently destroys and recreates the
	// database mid-test. A file has no such lifetime rule, and t.TempDir cleans
	// it up. Speed is irrelevant here; determinism is not.
	dsn := filepath.Join(t.TempDir(), "outbox.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// SQLite takes one writer at a time; the relayer and the assertions both
	// write, so serialise rather than surfacing SQLITE_BUSY as a flake.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := outbox.Migrate(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	return db
}

func makeEvent(id string) events.Event {
	return events.Event{
		ID:     id,
		Type:   "test.created",
		Source: "test",
		Time:   time.Now().UTC(),
	}
}

// runOneRelay runs the relay loop for one poll cycle using a very short interval.
func runOneRelay(t *testing.T, bus *outbox.Bus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	bus.Relay(outbox.RelayOptions{PollInterval: 1 * time.Millisecond}).Start(ctx) //nolint
}

// ── test doubles ──────────────────────────────────────────────────────────────

type nopPublisher struct{}

func (n *nopPublisher) Publish(_ context.Context, _ events.Event) error        { return nil }
func (n *nopPublisher) PublishBatch(_ context.Context, _ []events.Event) error { return nil }
func (n *nopPublisher) Close() error                                           { return nil }

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingPublisher) Publish(_ context.Context, e events.Event) error {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return nil
}
func (r *recordingPublisher) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := r.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (r *recordingPublisher) Close() error { return nil }
func (r *recordingPublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

type failingPublisher struct{ err error }

func (f *failingPublisher) Publish(_ context.Context, _ events.Event) error        { return f.err }
func (f *failingPublisher) PublishBatch(_ context.Context, _ []events.Event) error { return f.err }
func (f *failingPublisher) Close() error                                           { return nil }

// slowPublisher wraps a publisher and adds a fixed delay per Publish call so
// concurrent relayers have time to race on the same unshipped row.
type slowPublisher struct {
	events.Publisher
	delay time.Duration
}

func (s *slowPublisher) Publish(ctx context.Context, e events.Event) error {
	time.Sleep(s.delay)
	return s.Publisher.Publish(ctx, e)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestOutbox_TxCommit: inserting via PublishWithExecer inside a committed
// transaction leaves exactly one row in event_outbox.
func TestOutbox_TxCommit(t *testing.T) {
	db := openTestDB(t)
	bus := outbox.Wrap(&nopPublisher{}, db, "sqlite")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	e := makeEvent("commit-1")
	if err := bus.PublishWithExecer(context.Background(), tx, e); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM event_outbox WHERE id = ?", e.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after tx commit, got %d", count)
	}
}

// TestOutbox_TxRollback: inserting via PublishWithExecer inside a rolled-back
// transaction leaves no row in event_outbox.
func TestOutbox_TxRollback(t *testing.T) {
	db := openTestDB(t)
	bus := outbox.Wrap(&nopPublisher{}, db, "sqlite")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	e := makeEvent("rollback-1")
	if err := bus.PublishWithExecer(context.Background(), tx, e); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()

	var count int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM event_outbox WHERE id = ?", e.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after tx rollback, got %d", count)
	}
}

// TestOutbox_RelayMarksShipped: the relay delivers the event to the downstream
// publisher and sets shipped_at on the row.
func TestOutbox_RelayMarksShipped(t *testing.T) {
	db := openTestDB(t)
	rec := &recordingPublisher{}
	bus := outbox.Wrap(rec, db, "sqlite")

	e := makeEvent("ship-1")
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	runOneRelay(t, bus)

	if rec.count() != 1 {
		t.Fatalf("expected 1 downstream publish, got %d", rec.count())
	}

	var shippedAt sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		"SELECT shipped_at FROM event_outbox WHERE id = ?", e.ID).Scan(&shippedAt); err != nil {
		t.Fatal(err)
	}
	if !shippedAt.Valid {
		t.Fatal("expected shipped_at to be set after successful relay")
	}
}

// TestOutbox_RelayRetriesOnFailure: when the downstream publisher returns an
// error the relay increments attempts and leaves shipped_at NULL.
func TestOutbox_RelayRetriesOnFailure(t *testing.T) {
	db := openTestDB(t)
	bus := outbox.Wrap(&failingPublisher{err: errors.New("broker down")}, db, "sqlite")

	e := makeEvent("retry-1")
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	runOneRelay(t, bus)

	var attempts int
	var shippedAt sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		"SELECT attempts, shipped_at FROM event_outbox WHERE id = ?", e.ID).
		Scan(&attempts, &shippedAt); err != nil {
		t.Fatal(err)
	}
	if attempts < 1 {
		t.Fatalf("expected attempts >= 1 after failed relay, got %d", attempts)
	}
	if shippedAt.Valid {
		t.Fatal("expected shipped_at NULL after relay failure")
	}
}

// TestOutbox_RelaySkipsAlreadyShipped: the relay does not re-deliver events
// that have already been shipped.
func TestOutbox_RelaySkipsAlreadyShipped(t *testing.T) {
	db := openTestDB(t)
	rec := &recordingPublisher{}
	bus := outbox.Wrap(rec, db, "sqlite")

	e := makeEvent("skip-1")
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	runOneRelay(t, bus) // ships the event
	runOneRelay(t, bus) // must not ship again

	if n := rec.count(); n != 1 {
		t.Fatalf("expected 1 total delivery, got %d (relay should skip shipped rows)", n)
	}
}

// TestOutbox_ConcurrentRelayers_NoDoublePublish is a RED test for E7.
//
// Two relayers running simultaneously must not deliver the same event twice.
// This test currently FAILS because the relay issues a plain SELECT with no
// row-level locking (SELECT … FOR UPDATE SKIP LOCKED / polling-lease), so
// both relayers read the same unshipped row and both publish it.
//
// Fix (E7): add SELECT … FOR UPDATE SKIP LOCKED (Postgres) or a lease column
// (SQLite/MySQL) so at most one relayer claims each row per cycle.
func TestOutbox_ConcurrentRelayers_NoDoublePublish(t *testing.T) {
	db := openTestDB(t)
	rec := &recordingPublisher{}
	// Slow downstream so both relayers read the row before either marks it shipped.
	bus := outbox.Wrap(&slowPublisher{Publisher: rec, delay: 20 * time.Millisecond}, db, "sqlite")

	e := makeEvent("double-1")
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runOneRelay(t, bus)
		}()
	}
	wg.Wait()

	if n := rec.count(); n != 1 {
		t.Fatalf("concurrent relayers delivered %d copies of the same event; want 1 (E7: missing row-level locking)", n)
	}
}
