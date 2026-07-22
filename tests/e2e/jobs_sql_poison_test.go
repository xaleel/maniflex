package e2e

// Audit JB-11: one row whose payload would not decode made scanJobs return an
// error, so the whole Dequeue batch failed and no job dispatched.
//
// The claim had already committed by then — Dequeue claims with a single
// UPDATE ... RETURNING, so every row in the batch was already 'running' with an
// attempt spent — and nothing in this adapter reclaims a running row (the claim
// predicate is status IN ('enqueued','failed')). So every good job claimed
// alongside the bad one was stranded in 'running' permanently: never executed,
// never retried, never dead-lettered, invisible to the worker. One bad row
// silently destroyed up to a batch of good ones.
//
// Undecodable rows are now skipped and quarantined as dead, and the batch's good
// jobs are returned.
//
//	go test ./e2e/ -run TestJobsSQLPoison

import (
	"bytes"
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// selectiveCipher stores payloads verbatim but refuses to decrypt any marked
// POISON — a row whose bytes cannot be recovered (a rotated key, a corrupted
// value) sitting in a batch whose other rows decode fine.
type selectiveCipher struct{}

func (selectiveCipher) Encrypt(p []byte) ([]byte, error) { return p, nil }

func (selectiveCipher) Decrypt(c []byte) ([]byte, error) {
	if bytes.Contains(c, []byte("POISON")) {
		return nil, errors.New("cannot decrypt: key mismatch")
	}
	return c, nil
}

// poisonQueue migrates a dedicated table and returns a cipher-backed queue.
func poisonQueue(t *testing.T, db *stdsql.DB, table string) *jobssql.Queue {
	t.Helper()
	if err := jobssql.Migrate(context.Background(), db, "sqlite", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return jobssql.New(db, jobssql.WithTableName(table), jobssql.WithPayloadCipher(selectiveCipher{}))
}

func enqueueBody(t *testing.T, q *jobssql.Queue, body string) string {
	t.Helper()
	id, err := q.Enqueue(context.Background(), jobs.Job{
		Type:    "poison.test",
		Payload: json.RawMessage(`{"body":"` + body + `"}`),
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", body, err)
	}
	return id
}

func jobRowState(t *testing.T, db *stdsql.DB, table, id string) (status string, lastErr, lease stdsql.NullString) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT status, last_error, lease_until FROM `+table+` WHERE id=?`, id,
	).Scan(&status, &lastErr, &lease); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return status, lastErr, lease
}

// The headline: the good jobs claimed alongside a poison row are still delivered.
func TestJobsSQLPoison_GoodJobsInBatchStillDispatch(t *testing.T) {
	db := rawJobsDB(t)
	const table = "poison_batch"
	q := poisonQueue(t, db, table)

	for _, body := range []string{"good-1", "POISON", "good-2", "good-3"} {
		enqueueBody(t, q, body)
	}

	claimed, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("one undecodable row failed the whole batch: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("got %d jobs, want the 3 decodable ones (the good jobs must not be "+
			"stranded by their batch-mate)", len(claimed))
	}
	for _, j := range claimed {
		if bytes.Contains(j.Payload, []byte("POISON")) {
			t.Errorf("the undecodable job was dispatched anyway: %s", j.Payload)
		}
	}
}

// The poison row is quarantined: dead, with the decode failure recorded, lease
// cleared — not left sitting in 'running' where nothing would ever reclaim it.
func TestJobsSQLPoison_BadRowIsQuarantined(t *testing.T) {
	db := rawJobsDB(t)
	const table = "poison_quarantine"
	q := poisonQueue(t, db, table)

	goodID := enqueueBody(t, q, "good-1")
	badID := enqueueBody(t, q, "POISON")

	if _, err := q.Dequeue(context.Background(), 10); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	status, lastErr, lease := jobRowState(t, db, table, badID)
	if status != "dead" {
		t.Errorf("poison row status = %q, want \"dead\" (quarantined)", status)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Errorf("poison row has no last_error explaining the quarantine")
	}
	if lease.Valid && lease.String != "" {
		t.Errorf("poison row still holds a lease: %q", lease.String)
	}

	// The good job it was claimed with is running, as a claimed job should be.
	if s, _, _ := jobRowState(t, db, table, goodID); s != "running" {
		t.Errorf("good job status = %q, want \"running\"", s)
	}

	// A quarantined row is terminal: it is not handed out again.
	again, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("second dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("quarantined row was re-claimed: %d jobs", len(again))
	}
}

// The Inspector path must survive an undecodable payload too — it is how an
// operator finds the quarantined row, so erroring there would hide it.
func TestJobsSQLPoison_QuarantinedRowIsInspectable(t *testing.T) {
	db := rawJobsDB(t)
	const table = "poison_inspect"
	ctx := context.Background()
	q := poisonQueue(t, db, table)

	badID := enqueueBody(t, q, "POISON")
	enqueueBody(t, q, "good-1")
	if _, err := q.Dequeue(ctx, 10); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	states, err := q.List(ctx, jobs.ListQuery{})
	if err != nil {
		t.Fatalf("an undecodable payload broke the job listing: %v", err)
	}
	var found *jobs.JobState
	for i := range states {
		if states[i].Job.ID == badID {
			found = &states[i]
		}
	}
	if found == nil {
		t.Fatalf("quarantined row is missing from the listing (%d rows) — it must stay "+
			"visible, that is the point of quarantining it", len(states))
	}
	if found.Status != jobs.Status("dead") {
		t.Errorf("quarantined row listed with status %q, want dead", found.Status)
	}
	if found.Error == "" {
		t.Errorf("quarantined row listed with no error explaining it")
	}
}
