package events

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

// DedupeReleaser is an optional interface a DedupeStore may implement to undo a
// claim that did not result in successful processing.
//
// Seen is a check-and-record in one atomic step, which is what makes a store
// safe when two workers are handed the same event at once — but it means the ID
// is recorded on handler *entry*, before the outcome is known. Without Release,
// a handler that returns a transient error has already marked the event as
// processed, so DeliverWithRetry's next attempt is dropped as a duplicate and
// the delivery reports success: the event is never processed and never
// dead-lettered (audit EV-2).
//
// Both bundled stores implement this. A store that does not gets a warning from
// Dedupe when it is wrapped, because there is no correct fallback: the claim
// cannot be undone, so a transient failure still loses the event.
type DedupeReleaser interface {
	// Release removes a previously recorded id so it can be processed again.
	// Releasing an id that is not recorded is not an error.
	Release(ctx context.Context, id string) error
}

// Dedupe wraps a Handler with idempotent delivery semantics.
// Events whose ID is already recorded in store are silently dropped.
// Recommended with every at-least-once broker adapter:
//
//	bus.Subscribe(ctx, events.Subscription{
//	    Handler: events.Dedupe(store)(myHandler),
//	})
//
// The ID is claimed before the handler runs, so a concurrent delivery of the
// same event is dropped rather than processed twice. If the handler then fails,
// the claim is released so the retry can re-process it — see DedupeReleaser for
// what happens with a store that cannot release.
func Dedupe(store DedupeStore) func(Handler) Handler {
	releaser, canRelease := store.(DedupeReleaser)
	if !canRelease {
		slog.Default().Warn("events: dedupe store cannot release claims; "+
			"a handler that fails transiently will drop the event instead of retrying it",
			slog.String("store", fmt.Sprintf("%T", store)),
			slog.String("fix", "implement events.DedupeReleaser"))
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, e Event) error {
			seen, err := store.Seen(ctx, e.ID)
			if err != nil {
				return err
			}
			if seen {
				return nil
			}
			handlerErr := next(ctx, e)
			if handlerErr != nil && canRelease {
				// The claim was taken on entry but the event was not processed.
				// Release it so the caller's retry is not mistaken for a
				// duplicate. A failure to release is logged, not returned: the
				// handler's own error is the one worth propagating.
				if relErr := releaser.Release(ctx, e.ID); relErr != nil {
					slog.Default().Error("events: dedupe claim release failed; "+
						"the event will be treated as processed and its retry dropped",
						slog.String("id", e.ID),
						slog.String("error", relErr.Error()))
				}
			}
			return handlerErr
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

// Release drops id, so a later delivery is treated as new. See DedupeReleaser.
func (s *InMemoryDedupeStore) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, id)
	return nil
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

// Release deletes id's row, so a later delivery is treated as new.
// Deleting a row that is not there is not an error. See DedupeReleaser.
func (s *SQLDedupeStore) Release(ctx context.Context, id string) error {
	q := `DELETE FROM event_dedupe WHERE id = ?`
	if s.driver == "postgres" {
		q = `DELETE FROM event_dedupe WHERE id = $1`
	}
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("dedupe release: %w", err)
	}
	return nil
}
