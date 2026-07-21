package e2e

// Audit JB-4 / JB-9, adapter half: each Source implements Requeuer so the
// worker can return an unhandled-type job to the queue without spending a retry
// attempt (see jobs_unhandled_test.go for the worker-level bounding). These
// tests pin the per-adapter behaviour: the requeued job comes back claimable,
// its Header changes and its attempt count survive the round-trip, and any hold
// the delivery took (a GroupKey) is released.
//
//	go test ./e2e/ -run TestRequeue

import (
	"context"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	"github.com/xaleel/maniflex/jobs/inproc"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// inproc: Requeue must restore the entry to claimable, preserve the worker's
// header + attempt count, and release the GroupKey — which the re-claim proves,
// since a key still held would keep the requeued job out of the next Dequeue.
func TestRequeue_Inproc(t *testing.T) {
	q := inproc.New()
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, jobs.Job{ID: "j1", Type: "unknown", GroupKey: "k"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := q.Dequeue(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("dequeue: got %d err %v", len(got), err)
	}
	if got[0].Attempts != 1 {
		t.Fatalf("first delivery Attempts = %d, want 1", got[0].Attempts)
	}

	// The worker undoes the delivery's attempt and stamps the requeue counter.
	j := got[0]
	j.Attempts = 0
	j.Headers = map[string]string{jobs.HeaderUnhandledRequeues: "1"}
	if err := q.Requeue(ctx, j, 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	got2, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("re-dequeue: %v", err)
	}
	if len(got2) != 1 {
		t.Fatal("requeued job was not claimable again — the GroupKey was not released, or the entry was dropped")
	}
	if got2[0].Attempts != 1 {
		t.Errorf("re-delivery Attempts = %d, want 1 (stored 0, +1 on delivery) — the budget must not climb", got2[0].Attempts)
	}
	if got2[0].Headers[jobs.HeaderUnhandledRequeues] != "1" {
		t.Errorf("requeue counter lost across the round-trip: headers = %v", got2[0].Headers)
	}
}

// jobs/sql: Requeue rewrites the row to enqueued with the given attempts and
// headers, claimable again once not_before passes (delay 0 here).
func TestRequeue_SQL(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("rq_jobs")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName("rq_jobs"))

	if _, err := q.Enqueue(ctx, jobs.Job{ID: "j1", Type: "unknown"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := q.Dequeue(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("dequeue: got %d err %v", len(got), err)
	}
	if got[0].Attempts != 1 {
		t.Fatalf("first delivery Attempts = %d, want 1", got[0].Attempts)
	}

	j := got[0]
	j.Attempts = 0
	j.Headers = map[string]string{jobs.HeaderUnhandledRequeues: "2"}
	if err := q.Requeue(ctx, j, 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	// The row must be back to enqueued (not left running) with the stored count.
	var status string
	var attempts int
	if err := db.QueryRowContext(ctx,
		`SELECT status, attempts FROM rq_jobs WHERE id='j1'`).Scan(&status, &attempts); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "enqueued" {
		t.Errorf("status = %q after requeue, want enqueued (JB-18: not stuck running)", status)
	}
	if attempts != 0 {
		t.Errorf("stored attempts = %d, want 0 — the requeue must not spend the budget", attempts)
	}

	got2, err := q.Dequeue(ctx, 1)
	if err != nil || len(got2) != 1 {
		t.Fatalf("re-dequeue: got %d err %v", len(got2), err)
	}
	if got2[0].Attempts != 1 {
		t.Errorf("re-delivery Attempts = %d, want 1", got2[0].Attempts)
	}
	if got2[0].Headers[jobs.HeaderUnhandledRequeues] != "2" {
		t.Errorf("requeue counter lost: headers = %v", got2[0].Headers)
	}
}

// Requeue with a real delay must not be immediately claimable — the job waits,
// so requeuing does not hot-loop while a handler-worker is still coming up.
func TestRequeue_SQLHonoursDelay(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("rq_delay_jobs")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName("rq_delay_jobs"))

	if _, err := q.Enqueue(ctx, jobs.Job{ID: "j1", Type: "unknown"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, _ := q.Dequeue(ctx, 1)
	if len(got) != 1 {
		t.Fatal("dequeue got nothing")
	}
	if err := q.Requeue(ctx, got[0], time.Hour); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("re-dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("claimed a job requeued with a 1h delay; not_before was not honoured")
	}
}
