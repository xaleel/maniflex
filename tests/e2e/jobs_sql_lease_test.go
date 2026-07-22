package e2e

// Audit JB-10: the jobs/sql visibility timeout (how long a claimed job stays
// invisible before another Dequeue may reclaim it) was hard-coded to 5m with no
// option, and RenewLease assigned lease_until = now+d outright. The worker renews
// with a short horizon (LeaseRenew*3 = 90s), well under the 5m lease, so the first
// renewal *shortened* a freshly claimed job's lease to ~90s — making a stalled
// renewer's job reclaimable far sooner than the timeout promises.
//
// The timeout is now WithLeaseDuration, and RenewLease takes the later of the
// current lease and now+d (MAX/GREATEST), so a renewal can only extend.
//
//	go test ./e2e/ -run TestJobsSQLLease

import (
	"context"
	stdsql "database/sql"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// leaseUntil reads and parses the lease_until column of one job row. ok is false
// when it is NULL/empty. The stored form is fixed-width RFC3339 (audit JB-7), which
// time.RFC3339Nano parses.
func leaseUntil(t *testing.T, db *stdsql.DB, table, id string) (time.Time, bool) {
	t.Helper()
	var s stdsql.NullString
	if err := db.QueryRow(ph(`SELECT lease_until FROM `+table+` WHERE id=?`), id).Scan(&s); err != nil {
		t.Fatalf("read lease_until: %v", err)
	}
	if !s.Valid || s.String == "" {
		return time.Time{}, false
	}
	tm, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		t.Fatalf("parse lease_until %q: %v", s.String, err)
	}
	return tm, true
}

func claimOne(t *testing.T, q *jobssql.Queue) string {
	t.Helper()
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, jobs.Job{Type: "lease.test"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("dequeue: err=%v n=%d", err, len(claimed))
	}
	return claimed[0].ID
}

// WithLeaseDuration stamps the configured timeout on claim, not the 5m default.
func TestJobsSQLLease_WithLeaseDurationStampsCustomTimeout(t *testing.T) {
	db := rawJobsDB(t)
	const table = "lease_custom"
	if err := jobssql.Migrate(context.Background(), db, jobsDriver(), jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table), jobssql.WithLeaseDuration(90*time.Second))

	t0 := time.Now()
	id := claimOne(t, q)

	until, ok := leaseUntil(t, db, table, id)
	if !ok {
		t.Fatal("lease_until not set on claim")
	}
	if got := until.Sub(t0); got < 80*time.Second || got > 100*time.Second {
		t.Errorf("lease window = %v, want ~90s (the configured timeout), not the 5m default", got)
	}
}

// A renewal shorter than the current lease must leave it untouched (the JB-10
// bug); a renewal past it extends it.
func TestJobsSQLLease_RenewNeverShortens(t *testing.T) {
	db := rawJobsDB(t)
	const table = "lease_renew"
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, jobsDriver(), jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table)) // default 5m lease

	id := claimOne(t, q)
	before, ok := leaseUntil(t, db, table, id)
	if !ok {
		t.Fatal("no lease after claim")
	}

	// The worker's own horizon: LeaseRenew*3 = 90s, far below the 5m lease. It must
	// not shorten it.
	if err := q.RenewLease(ctx, id, 90*time.Second); err != nil {
		t.Fatalf("renew (short): %v", err)
	}
	if afterShort, _ := leaseUntil(t, db, table, id); !afterShort.Equal(before) {
		t.Errorf("a 90s renewal changed the 5m lease: %v → %v (renewal must never shorten)", before, afterShort)
	}

	// A horizon past the current lease genuinely extends it.
	if err := q.RenewLease(ctx, id, 10*time.Minute); err != nil {
		t.Fatalf("renew (long): %v", err)
	}
	if afterLong, _ := leaseUntil(t, db, table, id); !afterLong.After(before) {
		t.Errorf("a 10m renewal did not extend the 5m lease: %v → %v", before, afterLong)
	}
}
