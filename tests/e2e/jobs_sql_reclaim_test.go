package e2e

// Audit NEW-2: jobs/sql never reclaimed a 'running' row. The claim predicate is
// status IN ('enqueued','failed') and nothing moved a row back, so a worker that
// died mid-job left its jobs running forever — never redelivered, never retried,
// never dead-lettered, invisible to every later Dequeue. It also left the lease
// column with no reader: Dequeue's lease_until test only ever applied to rows
// that were already claimable, so WithLeaseDuration (JB-10) was inert for the
// visibility timeout it documents.
//
// Dequeue now sweeps expired leases first, as jobs/redis does with XAUTOCLAIM
// (JB-3), returning those rows to 'enqueued' — or dead-lettering them when the
// retry budget is spent.
//
//	go test ./e2e/ -run TestJobsSQLReclaim

import (
	"context"
	stdsql "database/sql"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// reclaimQueue migrates a private table and returns a queue with lease as its
// visibility timeout. A short lease also shortens the sweep's own throttle
// (lease/10), so these tests do not wait on a fixed interval.
func reclaimQueue(t *testing.T, table string, lease time.Duration) (*jobssql.Queue, *stdsql.DB) {
	t.Helper()
	db := rawJobsDB(t)
	if err := jobssql.Migrate(context.Background(), db, "sqlite", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return jobssql.New(db, jobssql.WithTableName(table), jobssql.WithLeaseDuration(lease)), db
}

func statusOf(t *testing.T, db *stdsql.DB, table, id string) (status, lastErr string) {
	t.Helper()
	if err := db.QueryRow(`SELECT status, last_error FROM `+table+` WHERE id=?`, id).
		Scan(&status, &lastErr); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status, lastErr
}

// The defect itself: a worker claims a job and dies without acking. Before the
// fix the row sat 'running' forever and no Dequeue ever saw it again.
func TestJobsSQLReclaim_CrashedWorkerJobComesBack(t *testing.T) {
	ctx := context.Background()
	q, db := reclaimQueue(t, "reclaim_basic", 50*time.Millisecond)

	id, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test", MaxRetry: 5})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: err=%v n=%d", err, len(claimed))
	}
	if claimed[0].Attempts != 1 {
		t.Fatalf("first claim attempts = %d, want 1", claimed[0].Attempts)
	}
	// The worker dies here: no Ack, no Nack, no RenewLease.
	if st, _ := statusOf(t, db, "reclaim_basic", id); st != "running" {
		t.Fatalf("status after claim = %q, want running", st)
	}

	time.Sleep(120 * time.Millisecond) // outlive the lease

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("the crashed worker's job was not redelivered (got %d jobs); it is stranded in 'running' forever", len(again))
	}
	if again[0].ID != id {
		t.Fatalf("redelivered %s, want %s", again[0].ID, id)
	}
	// The re-claim spends an attempt, which is what bounds a crash loop.
	if again[0].Attempts != 2 {
		t.Errorf("redelivered attempts = %d, want 2 — a reclaim must cost a retry or a crash loop never ends", again[0].Attempts)
	}
}

// A job whose worker died with the budget already spent is a poison pill: the
// next worker probably dies the same way, and since it never reaches Nack
// nothing else would ever stop the cycle.
func TestJobsSQLReclaim_SpentBudgetIsDeadLettered(t *testing.T) {
	ctx := context.Background()
	q, db := reclaimQueue(t, "reclaim_dead", 50*time.Millisecond)

	id, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test", MaxRetry: 1})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if claimed, err := q.Dequeue(ctx, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v n=%d", err, len(claimed))
	}
	// attempts is now 1, equal to MaxRetry. The worker dies.

	time.Sleep(120 * time.Millisecond)

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a job with no retry budget left was handed out again (%d jobs) — it would loop forever", len(again))
	}
	st, lastErr := statusOf(t, db, "reclaim_dead", id)
	if st != "dead" {
		t.Fatalf("status = %q, want dead", st)
	}
	if !strings.Contains(lastErr, "lease expired") {
		t.Errorf("last_error = %q, want it to explain the expired lease — nothing else records why this job died", lastErr)
	}
}

// Over-reach guard: a live worker's job must not be stolen while its lease holds.
func TestJobsSQLReclaim_LiveLeaseIsNotStolen(t *testing.T) {
	ctx := context.Background()
	q, _ := reclaimQueue(t, "reclaim_live", 5*time.Minute)

	if _, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if claimed, err := q.Dequeue(ctx, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v n=%d", err, len(claimed))
	}

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a running job with a live lease was reclaimed (%d jobs) — the sweep is stealing work from healthy workers", len(again))
	}
}

// The renewal path is what a long-running job relies on: it is alive, it keeps
// extending, and the sweep must respect that.
func TestJobsSQLReclaim_RenewedLeaseSurvives(t *testing.T) {
	ctx := context.Background()
	q, _ := reclaimQueue(t, "reclaim_renew", 50*time.Millisecond)

	if _, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v n=%d", err, len(claimed))
	}
	// Still working, so it renews well past the original lease.
	if err := q.RenewLease(ctx, claimed[0].ID, time.Hour); err != nil {
		t.Fatalf("renew: %v", err)
	}

	time.Sleep(120 * time.Millisecond) // past the original lease, not the renewed one

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a renewed lease was reclaimed anyway (%d jobs) — RenewLease cannot protect a long job", len(again))
	}
}

// The sweep must not resurrect a job that finished. The Worker writes its
// terminal Ack on a context detached from shutdown (JB-5), so that write can
// land around the time a lease lapses.
func TestJobsSQLReclaim_FinishedJobIsNotResurrected(t *testing.T) {
	ctx := context.Background()
	q, db := reclaimQueue(t, "reclaim_acked", 50*time.Millisecond)

	if _, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v n=%d", err, len(claimed))
	}
	if err := q.Ack(ctx, claimed[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	time.Sleep(120 * time.Millisecond)

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a succeeded job was re-run after its lease lapsed (%d jobs)", len(again))
	}
	if st, _ := statusOf(t, db, "reclaim_acked", claimed[0].ID); st != "succeeded" {
		t.Fatalf("status = %q, want succeeded", st)
	}
}

// A crashed worker used to wedge its whole GroupKey: the claim excludes any key
// with a running row, and that row never left 'running', so every later job of
// that key was blocked forever too.
func TestJobsSQLReclaim_CrashedWorkerDoesNotWedgeItsGroup(t *testing.T) {
	ctx := context.Background()
	q, _ := reclaimQueue(t, "reclaim_group", 50*time.Millisecond)

	for range 2 {
		if _, err := q.Enqueue(ctx, jobs.Job{Type: "reclaim.test", GroupKey: "tenant-a", MaxRetry: 5}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	claimed, err := q.Dequeue(ctx, 2)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: err=%v n=%d (group serialisation should yield exactly 1)", err, len(claimed))
	}
	// The worker holding tenant-a dies.

	time.Sleep(120 * time.Millisecond)

	again, err := q.Dequeue(ctx, 2)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(again) == 0 {
		t.Fatal("the group stayed blocked after its holder crashed — every job of that key is stranded")
	}
	if len(again) > 1 {
		t.Fatalf("claimed %d jobs of one GroupKey; the sweep must not break serialisation", len(again))
	}
}
