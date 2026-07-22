package e2e

// Audit JB-7: not_before / lease_until are compared as TEXT in SQLite, so the
// timestamp format must sort lexicographically the same way it sorts in time.
// jobs/sql now writes a fixed nine-digit fraction for that reason. These tests
// run the actual comparison through a real SQLite database, with timestamps
// hand-written in that fixed format, to confirm the database orders them
// chronologically across the whole-second/fractional boundary. (The format
// itself — that jobs/sql emits these fixed-width strings — is pinned by the
// unit tests in jobs/sql.)
//
//	go test ./e2e/ -run TestJobsSQLTimestamp

import (
	stdcontext "context"
	"testing"

	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

func insertTSJob(t *testing.T, table, id, notBefore string) func(now string) int {
	t.Helper()
	db := rawJobsDB(t)
	ctx := stdcontext.Background()
	if err := jobssql.Migrate(ctx, db, jobsDriver(), jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO `+table+` (id,type,not_before,created_at,updated_at) VALUES (?,?,?,?,?)`,
		id, "t", notBefore, notBefore, notBefore); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Returns how many rows are "due" (not_before <= now) at the given now.
	return func(now string) int {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE id=? AND not_before <= ?`, id, now).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
}

// A job scheduled on a whole second is due once a later, fractional instant in
// the same second arrives — the case the old variable-width format got wrong
// (the whole-second string sorted after the fractional one).
func TestJobsSQLTimestamp_WholeSecondJobIsDueAtFractionalLater(t *testing.T) {
	skipUnlessSQLite(t, "asserts SQLite's lexicographic TEXT comparison of timestamps; Postgres uses TIMESTAMPTZ and orders them natively")
	due := insertTSJob(t, "ts_whole", "j1", "2026-07-21T12:34:56.000000000Z")

	if n := due("2026-07-21T12:34:56.500000000Z"); n != 1 {
		t.Errorf("whole-second job not seen as due half a second later: count=%d, want 1", n)
	}
}

// The inverse: a job scheduled a fraction into a second must NOT be due at a
// whole-second instant earlier in that same second — no firing before its time.
func TestJobsSQLTimestamp_FractionalJobIsNotDueAtWholeSecondEarlier(t *testing.T) {
	skipUnlessSQLite(t, "asserts SQLite's lexicographic TEXT comparison of timestamps; Postgres uses TIMESTAMPTZ and orders them natively")
	due := insertTSJob(t, "ts_frac", "j1", "2026-07-21T12:34:56.500000000Z")

	if n := due("2026-07-21T12:34:56.000000000Z"); n != 0 {
		t.Errorf("job scheduled at .5s was claimed at .0s of the same second: count=%d, want 0", n)
	}
	// ...and it becomes due once its own instant is reached.
	if n := due("2026-07-21T12:34:56.500000000Z"); n != 1 {
		t.Errorf("job not due at its exact scheduled instant: count=%d, want 1", n)
	}
}
