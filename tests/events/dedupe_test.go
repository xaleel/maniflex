package events_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"maniflex/events"
	_ "modernc.org/sqlite"
)

// ── InMemoryDedupeStore ───────────────────────────────────────────────────────

func TestInMemoryDedupe_FirstCallReturnsFalse(t *testing.T) {
	s := events.NewInMemoryDedupeStore(10, time.Hour)
	seen, err := s.Seen(context.Background(), "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("expected Seen=false for a fresh ID")
	}
}

func TestInMemoryDedupe_SecondCallReturnsTrue(t *testing.T) {
	s := events.NewInMemoryDedupeStore(10, time.Hour)
	s.Seen(context.Background(), "id-1") //nolint:errcheck
	seen, err := s.Seen(context.Background(), "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("expected Seen=true for a previously recorded ID")
	}
}

func TestInMemoryDedupe_TTLExpiry(t *testing.T) {
	s := events.NewInMemoryDedupeStore(10, 1*time.Millisecond)
	s.Seen(context.Background(), "id-ttl") //nolint:errcheck
	time.Sleep(5 * time.Millisecond)       // let TTL expire

	seen, err := s.Seen(context.Background(), "id-ttl")
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("expected Seen=false after TTL expiry")
	}
}

func TestInMemoryDedupe_EvictionAtCapacity(t *testing.T) {
	const maxSize = 3
	s := events.NewInMemoryDedupeStore(maxSize, time.Hour)

	// Fill to capacity.
	for i := range maxSize {
		s.Seen(context.Background(), fmt.Sprintf("id-%d", i)) //nolint:errcheck
	}

	// Adding a new entry must evict the oldest and not panic or error.
	seen, err := s.Seen(context.Background(), "id-new")
	if err != nil {
		t.Fatalf("Seen after capacity: %v", err)
	}
	if seen {
		t.Fatal("id-new should be fresh even after eviction")
	}

	// Seen() is check-and-record: every call mutates the store, so the set of
	// surviving entries cannot be counted through the public API. What is sound
	// to assert is that eviction drops an *older* entry, never the newest — so
	// id-new must still be present immediately after being recorded.
	seen, err = s.Seen(context.Background(), "id-new")
	if err != nil {
		t.Fatalf("re-check id-new: %v", err)
	}
	if !seen {
		t.Fatal("id-new was evicted; eviction must drop an older entry, not the newest")
	}
}

func TestInMemoryDedupe_ConcurrentAccess(t *testing.T) {
	s := events.NewInMemoryDedupeStore(1000, time.Hour)
	done := make(chan struct{})
	for i := range 50 {
		go func(i int) {
			s.Seen(context.Background(), fmt.Sprintf("id-%d", i)) //nolint:errcheck
			done <- struct{}{}
		}(i)
	}
	for range 50 {
		<-done
	}
}

// ── Dedupe wrapper ────────────────────────────────────────────────────────────

func TestDedupe_DropsSeenEvents(t *testing.T) {
	store := events.NewInMemoryDedupeStore(10, time.Hour)
	var calls int
	wrapped := events.Dedupe(store)(func(_ context.Context, _ events.Event) error {
		calls++
		return nil
	})

	e := events.Event{ID: "dup-1", Type: "x", Time: time.Now()}
	wrapped(context.Background(), e) //nolint:errcheck
	wrapped(context.Background(), e) //nolint:errcheck
	wrapped(context.Background(), e) //nolint:errcheck

	if calls != 1 {
		t.Fatalf("expected handler called once, got %d (duplicates not dropped)", calls)
	}
}

func TestDedupe_PassesUniqueEvents(t *testing.T) {
	store := events.NewInMemoryDedupeStore(10, time.Hour)
	var calls int
	wrapped := events.Dedupe(store)(func(_ context.Context, _ events.Event) error {
		calls++
		return nil
	})

	for i := range 5 {
		e := events.Event{ID: fmt.Sprintf("u-%d", i), Type: "x", Time: time.Now()}
		wrapped(context.Background(), e) //nolint:errcheck
	}

	if calls != 5 {
		t.Fatalf("expected 5 handler calls for 5 unique events, got %d", calls)
	}
}

// ── SQLDedupeStore ────────────────────────────────────────────────────────────

func openDedupeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Single connection so all goroutines share the same in-memory database.
	// Without this, each sql.DB connection gets its own :memory: database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store := events.NewSQLDedupeStore(db, "sqlite")
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLDedupe_FirstCallReturnsFalse(t *testing.T) {
	db := openDedupeDB(t)
	store := events.NewSQLDedupeStore(db, "sqlite")
	seen, err := store.Seen(context.Background(), "sql-1")
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("expected Seen=false for a fresh ID")
	}
}

func TestSQLDedupe_SecondCallReturnsTrue(t *testing.T) {
	db := openDedupeDB(t)
	store := events.NewSQLDedupeStore(db, "sqlite")
	store.Seen(context.Background(), "sql-1") //nolint:errcheck

	seen, err := store.Seen(context.Background(), "sql-1")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("expected Seen=true for previously recorded ID")
	}
}

// TestSQLDedupe_UniqueConstraintIsDuplicate verifies that a raw INSERT
// conflict (simulating a concurrent process inserting the same ID) is treated
// as a duplicate, not an error.
func TestSQLDedupe_UniqueConstraintIsDuplicate(t *testing.T) {
	db := openDedupeDB(t)
	store := events.NewSQLDedupeStore(db, "sqlite")

	// Insert directly to simulate a concurrent writer.
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO event_dedupe (id, seen_at) VALUES (?, ?)",
		"concurrent-id", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	seen, err := store.Seen(context.Background(), "concurrent-id")
	if err != nil {
		t.Fatalf("expected no error on constraint conflict, got: %v", err)
	}
	if !seen {
		t.Fatal("expected Seen=true when row was already present (constraint conflict)")
	}
}

func TestSQLDedupe_ConcurrentInserts(t *testing.T) {
	db := openDedupeDB(t)
	store := events.NewSQLDedupeStore(db, "sqlite")

	// All goroutines try to mark the same ID. Exactly one should return false.
	const n = 10
	results := make(chan bool, n)
	for range n {
		go func() {
			seen, _ := store.Seen(context.Background(), "race-id")
			results <- seen
		}()
	}

	var falseCount int
	for range n {
		if !<-results {
			falseCount++
		}
	}
	if falseCount != 1 {
		t.Fatalf("expected exactly 1 Seen=false (first insert) among concurrent goroutines, got %d", falseCount)
	}
}
