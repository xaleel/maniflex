// Package outbox provides a transactional outbox publisher for maniflex events.
//
// Events are durably written to an event_outbox table within the same database
// transaction as the business write, then relayed to a downstream Publisher by
// a background Relayer. This eliminates the dual-write problem: if the business
// transaction rolls back, no event is produced; if the broker is temporarily
// unavailable, the event is retried from the outbox.
//
// Quick start:
//
//	import "github.com/xaleel/maniflex/events/outbox"
//
//	// 1. Create the outbox table (once, at startup or migration time):
//	if err := outbox.Migrate(ctx, rawDB, "sqlite"); err != nil { ... }
//
//	// 2. Wrap the downstream publisher:
//	bus := outbox.Wrap(redisBus, rawDB, "sqlite")
//
//	// 3. Register events.Emit with the outbox bus:
//	server.Pipeline.DB.Register(
//	    events.Emit(bus),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.AtPosition(maniflex.After),
//	)
//
//	// 4. Start the relay in a background goroutine:
//	go bus.Relay(outbox.RelayOptions{}).Start(ctx)
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"time"

	"github.com/xaleel/maniflex/events"
)

// Migrate creates the event_outbox table if it does not exist.
// driver is "postgres" or "sqlite"; Postgres uses JSONB and TIMESTAMPTZ,
// SQLite uses TEXT and TIMESTAMP.
// Call this once at startup or as part of your schema migrations.
func Migrate(ctx context.Context, db events.SQLExecer, driver string) error {
	payloadType := "TEXT"
	tsType := "TIMESTAMP"
	if driver == "postgres" {
		payloadType = "JSONB"
		tsType = "TIMESTAMPTZ"
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS event_outbox (
			id              TEXT PRIMARY KEY,
			type            TEXT NOT NULL,
			payload         %s NOT NULL,
			created_at      %s NOT NULL,
			shipped_at      %s,
			next_attempt_at %s,
			lease_until     %s,
			lease_id        TEXT,
			attempts        INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT
		)`, payloadType, tsType, tsType, tsType, tsType))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS event_outbox_unshipped
		ON event_outbox (created_at)
		WHERE shipped_at IS NULL`)
	return err
}

// Bus is a transactional outbox publisher. It writes events to event_outbox
// within the business transaction so they are relayed durably after commit.
//
// Implements events.Publisher and events.TxPublisher.
type Bus struct {
	downstream events.Publisher
	db         querier
	driver     string
}

// querier is satisfied by *sql.DB and any type that exposes ExecContext + QueryContext.
type querier interface {
	events.SQLExecer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Wrap creates an outbox Bus that durably stores events in db and relays
// them via downstream. driver is "postgres" or "sqlite".
//
// Call Migrate before Wrap.
func Wrap(downstream events.Publisher, db querier, driver string) *Bus {
	return &Bus{downstream: downstream, db: db, driver: driver}
}

// Publish inserts the event into event_outbox outside a transaction.
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	return b.insert(ctx, b.db, e)
}

// PublishWithExecer inserts the event within an existing database transaction.
// Called automatically by events.Emit when the active Tx implements events.SQLExecer.
func (b *Bus) PublishWithExecer(ctx context.Context, ex events.SQLExecer, e events.Event) error {
	return b.insert(ctx, ex, e)
}

// PublishBatch inserts all events in es into the outbox.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Close is a no-op; close the underlying *sql.DB through the adapter.
func (b *Bus) Close() error { return nil }

func (b *Bus) insert(ctx context.Context, ex events.SQLExecer, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("outbox: marshal: %w", err)
	}
	q := fmt.Sprintf(
		`INSERT INTO event_outbox (id, type, payload, created_at) VALUES (%s, %s, %s, %s)`,
		b.ph(1), b.ph(2), b.ph(3), b.ph(4))
	if _, err := ex.ExecContext(ctx, q, e.ID, e.Type, string(payload), time.Now().UTC()); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

func (b *Bus) ph(n int) string {
	if b.driver == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// ── Relay ─────────────────────────────────────────────────────────────────────

// RelayOptions configures the Relayer.
type RelayOptions struct {
	// PollInterval is how often the relay checks for unshipped events. Default: 1s.
	PollInterval time.Duration
	// BatchSize is the number of events fetched per poll cycle. Default: 100.
	BatchSize int
	// MaxAttempts is the maximum number of delivery attempts before a row is
	// abandoned (or sent to DLQType). Default: 10.
	MaxAttempts int
	// DLQType is the event Type published after MaxAttempts is exhausted.
	// Empty string disables dead-lettering; the row is marked shipped and dropped.
	DLQType string
	// RetainShipped controls how long successfully shipped rows are kept.
	// Rows with shipped_at older than this duration are deleted periodically.
	// Default: 0 (keep forever). The sweep runs every max(PollInterval×60, 1m).
	RetainShipped time.Duration
}

// Relayer polls event_outbox and ships unshipped rows to the downstream Publisher.
type Relayer struct {
	bus  *Bus
	opts RelayOptions
}

// Relay returns a Relayer that pumps unshipped outbox rows to the downstream publisher.
// Call Relayer.Start(ctx) in a background goroutine.
func (b *Bus) Relay(opts RelayOptions) *Relayer {
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Second
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 100
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 10
	}
	return &Relayer{bus: b, opts: opts}
}

// Start runs the relay loop until ctx is cancelled. Returns ctx.Err() on exit.
func (r *Relayer) Start(ctx context.Context) error {
	var sweepTick <-chan time.Time
	if r.opts.RetainShipped > 0 {
		sweepInterval := r.opts.PollInterval * 60
		if sweepInterval < time.Minute {
			sweepInterval = time.Minute
		}
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		sweepTick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.opts.PollInterval):
			_ = r.relay(ctx) // log errors externally; transient failures should not stop the relay
		case <-sweepTick:
			_ = r.sweep(ctx)
		}
	}
}

type pending struct {
	id       string
	payload  string
	attempts int
}

func (r *Relayer) relay(ctx context.Context) error {
	var lbuf [8]byte
	if _, err := io.ReadFull(rand.Reader, lbuf[:]); err != nil {
		return fmt.Errorf("outbox: lease id: %w", err)
	}
	leaseID := fmt.Sprintf("%x", lbuf)
	now := time.Now().UTC()
	leaseUntil := now.Add(30 * time.Second)

	if err := r.claimBatch(ctx, leaseID, leaseUntil, now); err != nil {
		return err
	}

	b := r.bus
	rs, err := b.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, payload, attempts FROM event_outbox WHERE lease_id = %s AND shipped_at IS NULL`, b.ph(1)),
		leaseID)
	if err != nil {
		return err
	}

	var batch []pending
	for rs.Next() {
		var p pending
		if err := rs.Scan(&p.id, &p.payload, &p.attempts); err != nil {
			rs.Close()
			return err
		}
		batch = append(batch, p)
	}
	rs.Close()

	for _, p := range batch {
		r.processRow(ctx, p)
	}
	return nil
}

// claimBatch atomically assigns lease_id / lease_until to the next eligible batch.
// Postgres uses FOR UPDATE SKIP LOCKED; SQLite relies on its serialised writes.
func (r *Relayer) claimBatch(ctx context.Context, leaseID string, leaseUntil, now time.Time) error {
	b := r.bus
	var q string
	var args []any
	if b.driver == "postgres" {
		q = `
			WITH claimed AS (
				SELECT id FROM event_outbox
				WHERE shipped_at IS NULL
				  AND (lease_until IS NULL OR lease_until < $1)
				  AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
				  AND attempts < $3
				ORDER BY created_at
				LIMIT $4
				FOR UPDATE SKIP LOCKED
			)
			UPDATE event_outbox SET lease_until = $5, lease_id = $6
			WHERE id IN (SELECT id FROM claimed)`
		args = []any{now, now, r.opts.MaxAttempts, r.opts.BatchSize, leaseUntil, leaseID}
	} else {
		q = `UPDATE event_outbox SET lease_until = ?, lease_id = ?
			WHERE id IN (
				SELECT id FROM event_outbox
				WHERE shipped_at IS NULL
				  AND (lease_until IS NULL OR lease_until < ?)
				  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
				  AND attempts < ?
				ORDER BY created_at LIMIT ?
			)`
		args = []any{leaseUntil, leaseID, now, now, r.opts.MaxAttempts, r.opts.BatchSize}
	}
	_, err := b.db.ExecContext(ctx, q, args...)
	return err
}

// processRow delivers one pending row to the downstream publisher and updates
// the outbox row accordingly. On exhaustion it routes to the DLQ if configured.
func (r *Relayer) processRow(ctx context.Context, p pending) {
	var e events.Event
	if err := json.Unmarshal([]byte(p.payload), &e); err != nil {
		r.markError(ctx, p.id, err.Error(), p.attempts)
		return
	}
	if err := r.bus.downstream.Publish(ctx, e); err != nil {
		r.markError(ctx, p.id, err.Error(), p.attempts)
		if p.attempts+1 >= r.opts.MaxAttempts {
			r.routeToDLQ(ctx, e)
			r.markShipped(ctx, p.id)
		}
		return
	}
	r.markShipped(ctx, p.id)
}

// routeToDLQ re-publishes an event that exhausted its delivery attempts under
// the configured dead-letter type, carrying the original headers plus enough
// provenance to trace it back.
//
// Deliberately the same payload contract as events.DeliverWithRetry, which this
// used to diverge from in two ways (audit 11D.10). It stamped original_type but
// not original_id, so a dead-letter could not be tied to the event it came from;
// and it reused the original ID, which is worse — the original was already
// published under that ID, so any downstream deduper treats the dead-letter as a
// duplicate and drops it. The failure would then be dead-lettered into nothing,
// which is the one outcome a DLQ exists to prevent.
func (r *Relayer) routeToDLQ(ctx context.Context, e events.Event) {
	if r.opts.DLQType == "" {
		return
	}
	dlq := e
	dlq.ID = events.NewID() // fresh ID so downstream dedupers see a new event
	dlq.Type = r.opts.DLQType
	newHeaders := make(map[string]string, len(e.Headers)+2)
	maps.Copy(newHeaders, e.Headers)
	newHeaders["original_type"] = e.Type
	newHeaders["original_id"] = e.ID
	dlq.Headers = newHeaders
	if err := r.bus.downstream.Publish(ctx, dlq); err != nil {
		// Logged, not swallowed: this is the last chance to record the event
		// before it is marked shipped and forgotten.
		slog.Default().Error("outbox: DLQ publish failed, event lost",
			slog.String("dlq_type", r.opts.DLQType),
			slog.String("original_type", e.Type),
			slog.String("original_id", e.ID),
			slog.String("error", err.Error()))
	}
}

func (r *Relayer) markShipped(ctx context.Context, id string) {
	b := r.bus
	q := fmt.Sprintf(`UPDATE event_outbox SET shipped_at = %s, lease_until = NULL, lease_id = NULL WHERE id = %s`, b.ph(1), b.ph(2))
	_, _ = b.db.ExecContext(ctx, q, time.Now().UTC(), id)
}

func (r *Relayer) markError(ctx context.Context, id, errMsg string, attempts int) {
	b := r.bus
	next := time.Now().UTC().Add(relayBackoff(attempts + 1))
	q := fmt.Sprintf(
		`UPDATE event_outbox SET attempts = attempts + 1, last_error = %s, next_attempt_at = %s, lease_until = NULL, lease_id = NULL WHERE id = %s`,
		b.ph(1), b.ph(2), b.ph(3))
	_, _ = b.db.ExecContext(ctx, q, errMsg, next, id)
}

func (r *Relayer) sweep(ctx context.Context) error {
	b := r.bus
	cutoff := time.Now().UTC().Add(-r.opts.RetainShipped)
	q := fmt.Sprintf(`DELETE FROM event_outbox WHERE shipped_at IS NOT NULL AND shipped_at < %s`, b.ph(1))
	_, err := b.db.ExecContext(ctx, q, cutoff)
	return err
}

// relayBackoff returns an exponential back-off duration capped at 10 minutes.
// attempt is 1-based (first failure → attempt=1 → 1 s, second → 2 s, …).
func relayBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	const maxBackoff = 10 * time.Minute
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
