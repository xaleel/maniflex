package events

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// DedupeStore records seen event IDs for idempotent delivery.
// Implementations ship in this package: InMemoryDedupeStore and SQLDedupeStore.
type DedupeStore interface {
	// Seen reports whether id was processed before and marks it as seen atomically.
	// Returns (true, nil) for duplicates. Returns (false, nil) for new events.
	Seen(ctx context.Context, id string) (bool, error)
}

// Dedupe wraps a Handler with idempotent delivery semantics.
// Events whose ID is already recorded in store are silently dropped.
// Recommended with every at-least-once broker adapter:
//
//	bus.Subscribe(ctx, events.Subscription{
//	    Handler: events.Dedupe(store)(myHandler),
//	})
func Dedupe(store DedupeStore) func(Handler) Handler {
	return func(next Handler) Handler {
		return func(ctx context.Context, e Event) error {
			seen, err := store.Seen(ctx, e.ID)
			if err != nil {
				return err
			}
			if seen {
				return nil
			}
			return next(ctx, e)
		}
	}
}

// InMemoryDedupeStore is a thread-safe in-memory dedupe store with bounded size
// and per-entry TTL. Entries are evicted when full (LRU-style, O(n) eviction).
//
// This store is lost on process restart. Use SQLDedupeStore for persistence.
type InMemoryDedupeStore struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	maxSize int
	ttl     time.Duration
}

// NewInMemoryDedupeStore creates a store that holds up to maxSize IDs for ttl.
// Typical values: maxSize=10_000, ttl=24*time.Hour.
func NewInMemoryDedupeStore(maxSize int, ttl time.Duration) *InMemoryDedupeStore {
	return &InMemoryDedupeStore{
		seen:    make(map[string]time.Time, maxSize),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

func (s *InMemoryDedupeStore) Seen(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	if t, ok := s.seen[id]; ok {
		if now.Before(t.Add(s.ttl)) {
			return true, nil
		}
		delete(s.seen, id) // entry expired; treat as new
	}

	// Evict the oldest entry when at capacity.
	if len(s.seen) >= s.maxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.seen {
			if oldestKey == "" || v.Before(oldestTime) {
				oldestKey, oldestTime = k, v
			}
		}
		delete(s.seen, oldestKey)
	}

	s.seen[id] = now
	return false, nil
}

// SQLDedupeStore is a *sql.DB-backed dedupe store.
// Call Migrate to create the required table before use.
type SQLDedupeStore struct {
	db     *sql.DB
	driver string // "postgres" or "sqlite"
}

// NewSQLDedupeStore creates a store backed by db. driver is "postgres" or "sqlite".
func NewSQLDedupeStore(db *sql.DB, driver string) *SQLDedupeStore {
	return &SQLDedupeStore{db: db, driver: driver}
}

// Migrate creates the event_dedupe table if it does not exist.
func (s *SQLDedupeStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS event_dedupe (
			id       TEXT PRIMARY KEY,
			seen_at  TIMESTAMP NOT NULL
		)`)
	return err
}

// Seen atomically inserts id; returns true if a row already existed.
func (s *SQLDedupeStore) Seen(ctx context.Context, id string) (bool, error) {
	var q string
	if s.driver == "postgres" {
		q = `INSERT INTO event_dedupe (id, seen_at) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`
	} else {
		q = `INSERT INTO event_dedupe (id, seen_at) VALUES (?, ?) ON CONFLICT (id) DO NOTHING`
	}
	res, err := s.db.ExecContext(ctx, q, id, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("dedupe: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 0, nil // 0 rows affected → row already existed → duplicate
}
