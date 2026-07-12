package sqlite

// FindByIDForUpdate / LockForUpdate on SQLite is documented to rely on
// BEGIN IMMEDIATE, but Open never asked for it: BeginTx issued a deferred BEGIN
// and the write-skew protection came only from the write pool being capped at
// one connection. Resize that pool and a read-then-write transaction pair loses
// its serialisation (BUG-17).
//
// The test therefore uses two independent write connections — exactly what the
// cap was hiding — and checks that the pair still serialises.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// writeConn opens a single connection carrying the DSN the adapter's write pool
// uses. Two of these stand in for a write pool that someone resized.
func writeConn(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", withTxLockImmediate(withPragmas(path)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTxLockImmediate_SerialisesReadThenWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lock.db")

	a, b := writeConn(t, path), writeConn(t, path)
	for _, stmt := range []string{
		`CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`,
		`INSERT INTO counter (id, n) VALUES (1, 0)`,
	} {
		if _, err := a.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	// A opens a transaction and reads the row it is about to write — the shape of
	// LockForUpdate, the optimistic-lock check, and mfx:"lock_scope".
	txA, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("A begin: %v", err)
	}
	defer txA.Rollback() //nolint:errcheck // no-op after Commit
	var seenByA int
	if err := txA.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&seenByA); err != nil {
		t.Fatalf("A read: %v", err)
	}

	// B tries the same thing concurrently.
	began := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- increment(ctx, b, began) }()

	// With an immediate BEGIN, B is parked at its BEGIN while A holds the write
	// lock, so `began` stays open and the timer releases us. With a deferred one,
	// B is already inside its transaction reading the same stale row — the bug —
	// and `began` fires at once.
	select {
	case <-began:
	case <-time.After(250 * time.Millisecond):
	}

	if _, err := txA.ExecContext(ctx, `UPDATE counter SET n = ? WHERE id = 1`, seenByA+1); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}

	// A deferred B fails here instead: its snapshot went stale when A committed,
	// and SQLITE_BUSY on that upgrade is not something busy_timeout can retry away.
	if err := <-done; err != nil {
		t.Fatalf("B: %v", err)
	}

	var n int
	if err := a.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("final read: %v", err)
	}
	if n != 2 {
		t.Errorf("counter = %d, want 2 — B overwrote A's increment instead of seeing it", n)
	}
}

// increment runs the read-then-write pair in one transaction, closing began once
// its BEGIN has returned.
func increment(ctx context.Context, db *sql.DB, began chan<- struct{}) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	close(began)

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE counter SET n = ? WHERE id = 1`, n+1); err != nil {
		return err
	}
	return tx.Commit()
}
