package e2e

// Audit JB-2: GroupKey promises "at most one job per key runs at a time", but
// jobs/sql enforced it only with a WHERE clause — group_key NOT IN (SELECT ...
// WHERE status='running'). That subquery sees the state before the claim's own
// UPDATE, where none of the batch's jobs are running yet, so:
//
//   - Within one Dequeue, several enqueued jobs of the same key all pass the
//     check and are all stamped running by the single UPDATE.
//   - Across concurrent Dequeue transactions on Postgres, neither sees the
//     other's uncommitted claim, so each takes a job of the same key.
//
// The fix ranks candidates with ROW_NUMBER() OVER (PARTITION BY group_key) and
// claims only rank 1 per key, which closes the batch case on both drivers, and
// adds a partial unique index on (group_key) WHERE status='running' so the
// concurrency case is refused by the database rather than merely discouraged.
//
// SQLite serialises writes, so its concurrency is covered by the same lock that
// covers everything else; these tests exercise the batch case and the index
// guarantee directly. The Postgres concurrent-claim retry path has no server
// here and is verified by reading.
//
//	go test ./e2e/ -run TestJobsSQLGroup

import (
	stdsql "database/sql"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

func seedGroupJobs(t *testing.T, db *stdsql.DB, table, groupKey string, n int) *jobssql.Queue {
	t.Helper()
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate %s: %v", table, err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table))
	for i := range n {
		if _, err := q.Enqueue(ctx, jobs.Job{
			Type:     "grp.test",
			GroupKey: groupKey,
			Payload:  json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	return q
}

// The batch case. Three enqueued jobs of one key; a single Dequeue asking for
// all three must hand back exactly one, because the other two would be a second
// concurrent runner of that key.
func TestJobsSQLGroup_OneBatchClaimsOnlyOnePerKey(t *testing.T) {
	db := rawJobsDB(t)
	q := seedGroupJobs(t, db, "grp_batch_jobs", "cust-1", 3)

	got, err := q.Dequeue(context.Background(), 3)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("claimed %d jobs of one key in a single batch, want 1", len(got))
	}
}

// The next job of a key becomes claimable only once the running one is resolved
// — otherwise the fix would just wedge the key permanently.
func TestJobsSQLGroup_NextRunsAfterAck(t *testing.T) {
	db := rawJobsDB(t)
	q := seedGroupJobs(t, db, "grp_seq_jobs", "cust-1", 3)
	ctx := context.Background()

	first, err := q.Dequeue(ctx, 3)
	if err != nil || len(first) != 1 {
		t.Fatalf("first dequeue: got %d err %v", len(first), err)
	}

	// A second claim while the first is still running gets nothing for the key.
	blocked, err := q.Dequeue(ctx, 3)
	if err != nil {
		t.Fatalf("second dequeue: %v", err)
	}
	if len(blocked) != 0 {
		t.Errorf("claimed %d while a job of the key was already running, want 0", len(blocked))
	}

	if err := q.Ack(ctx, first[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	next, err := q.Dequeue(ctx, 3)
	if err != nil || len(next) != 1 {
		t.Fatalf("after ack: got %d err %v, want 1", len(next), err)
	}
	if next[0].ID == first[0].ID {
		t.Error("re-claimed the same job after it was acked")
	}
}

// Empty GroupKey means "no serialisation": those jobs must never be deduped
// against each other, or every unkeyed job would funnel through one-at-a-time.
func TestJobsSQLGroup_EmptyKeyJobsAreNotSerialised(t *testing.T) {
	db := rawJobsDB(t)
	q := seedGroupJobs(t, db, "grp_empty_jobs", "", 5)

	got, err := q.Dequeue(context.Background(), 5)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("claimed %d unkeyed jobs, want all 5 — empty keys must not be serialised", len(got))
	}
}

// A batch spanning several keys claims one of each in a single call — the fix
// must not collapse throughput to one job per Dequeue.
func TestJobsSQLGroup_DistinctKeysClaimedTogether(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("grp_multi_jobs")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName("grp_multi_jobs"))

	// Two jobs each for three keys.
	for _, key := range []string{"a", "b", "c"} {
		for i := range 2 {
			if _, err := q.Enqueue(ctx, jobs.Job{
				Type: "grp.test", GroupKey: key,
				Payload: json.RawMessage(fmt.Sprintf(`{"k":%q,"i":%d}`, key, i)),
			}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
	}

	got, err := q.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("claimed %d jobs across 3 keys, want 3 (one per key)", len(got))
	}
	keys := map[string]int{}
	for _, j := range got {
		keys[j.GroupKey]++
	}
	for k, c := range keys {
		if c != 1 {
			t.Errorf("key %q claimed %d times in one batch, want 1", k, c)
		}
	}
}

// Concurrent workers hammering one queue that mixes keyed and unkeyed jobs must
// never run two jobs of a key at once. On SQLite the write lock serialises
// claims, so this exercises the batch dedup under real contention; the same
// invariant is what the partial unique index guarantees on Postgres.
func TestJobsSQLGroup_ConcurrentWorkersNeverDoubleRunAKey(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("grp_conc_jobs")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName("grp_conc_jobs"))

	// 4 keys, 10 jobs each, plus a live-running tracker per key.
	keys := []string{"k1", "k2", "k3", "k4"}
	for _, k := range keys {
		for i := range 10 {
			if _, err := q.Enqueue(ctx, jobs.Job{
				Type: "grp.test", GroupKey: k,
				Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
	}

	var (
		mu       sync.Mutex
		running  = map[string]int{}
		maxSeen  = map[string]int{}
		violated bool
	)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				got, err := q.Dequeue(ctx, 3)
				if err != nil {
					t.Errorf("dequeue: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				for _, j := range got {
					mu.Lock()
					running[j.GroupKey]++
					if running[j.GroupKey] > maxSeen[j.GroupKey] {
						maxSeen[j.GroupKey] = running[j.GroupKey]
					}
					if running[j.GroupKey] > 1 {
						violated = true
					}
					mu.Unlock()

					// Simulate handling, then complete so the key frees up.
					if err := q.Ack(ctx, j.ID); err != nil {
						t.Errorf("ack: %v", err)
					}
					mu.Lock()
					running[j.GroupKey]--
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()

	if violated {
		t.Errorf("a key had more than one job running at once: peak per key = %v", maxSeen)
	}
}

// The guarantee behind the concurrency fix: the partial unique index makes two
// running jobs of one key unrepresentable. Proven directly, since the SQLite
// write lock means the concurrent-claim path never actually trips it here — the
// index is what protects Postgres, where two claims can overlap.
func TestJobsSQLGroup_IndexForbidsTwoRunningPerKey(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName("grp_idx_jobs")); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	insert := func(id, key, status string) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO grp_idx_jobs (id,type,payload,status,not_before,group_key,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			id, "t", "{}", status, "2020-01-01T00:00:00Z", key, "2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z")
		return err
	}

	if err := insert("a", "cust-1", "running"); err != nil {
		t.Fatalf("first running row rejected: %v", err)
	}
	// A second running row for the same key must be refused by the index.
	if err := insert("b", "cust-1", "running"); err == nil {
		t.Error("index allowed two running jobs for one key — the concurrency guarantee is absent")
	}
	// A second running row for a DIFFERENT key is fine.
	if err := insert("c", "cust-2", "running"); err != nil {
		t.Errorf("index wrongly rejected a different key: %v", err)
	}
	// Two running rows with EMPTY key are fine — empty means unserialised.
	if err := insert("d", "", "running"); err != nil {
		t.Fatalf("first empty-key running row rejected: %v", err)
	}
	if err := insert("e", "", "running"); err != nil {
		t.Errorf("index wrongly serialised empty-key jobs: %v", err)
	}
}
