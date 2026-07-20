package e2e

// Audit JB-1: the SQLite Dequeue claims jobs in two separate statements —
// an UPDATE that stamps lease_until, then a SELECT that re-finds "the rows we
// just claimed" by WHERE lease_until = <that same string>.
//
// lease_until is the only thing identifying the claim, and it is derived from
// time.Now(). Two workers whose clocks read the same value compute the same
// string, so each one's SELECT matches the other's rows as well as its own and
// both can scan the same job. The UPDATE being serialised by SQLite's write
// lock does not help: the serialisation does not span the two statements.
//
// A same-nanosecond read sounds unlikely until you notice the platform. Windows'
// system clock granularity is coarse (~0.5-15ms), so back-to-back time.Now()
// calls routinely return an identical value — and RFC3339Nano drops trailing
// zeros, so those reads render as one string.
//
//	go test ./e2e/ -run TestJobsSQLClaim

import (
	stdsql "database/sql"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// seedJobs migrates a dedicated table and enqueues n ready jobs.
func seedJobs(t *testing.T, db *stdsql.DB, table string, n int) *jobssql.Queue {
	t.Helper()
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, "sqlite", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate %s: %v", table, err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table))
	for i := range n {
		if _, err := q.Enqueue(ctx, jobs.Job{
			Type:    "claim.test",
			Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	return q
}

// The JB-1 regression. A job handed to two workers is executed twice, which is
// the guarantee a queue exists to provide.
func TestJobsSQLClaim_ConcurrentDequeueNeverDoubleClaims(t *testing.T) {
	db := rawJobsDB(t)
	const (
		workers  = 8
		perBatch = 4
		total    = 200
	)
	q := seedJobs(t, db, "claim_jobs", total)

	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
	)

	for range workers {
		wg.Go(func() {
			for {
				got, err := q.Dequeue(context.Background(), perBatch)
				if err != nil {
					t.Errorf("dequeue: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, j := range got {
					claimed[j.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	var dupes []string
	for id, n := range claimed {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s claimed %d times", id, n))
		}
	}
	if len(dupes) > 0 {
		t.Errorf("%d job(s) claimed more than once — each runs that many times:\n  %v",
			len(dupes), dupes)
	}
}

// The same defect without any concurrency: two sequential Dequeue calls close
// enough together to share a clock reading. The second call's SELECT matches
// the first call's rows, so it returns jobs it did not claim — and on a coarse
// clock this needs no unusual timing at all.
func TestJobsSQLClaim_BackToBackDequeuesReturnDistinctJobs(t *testing.T) {
	db := rawJobsDB(t)
	q := seedJobs(t, db, "claim_seq_jobs", 40)
	ctx := context.Background()

	seen := map[string]int{}
	for range 10 {
		got, err := q.Dequeue(ctx, 2)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		for _, j := range got {
			seen[j.ID]++
		}
	}

	for id, n := range seen {
		if n > 1 {
			t.Errorf("job %s returned by %d separate Dequeue calls", id, n)
		}
	}
}

// Anti-vacuity: a fix that claims nothing, or that hands back fewer jobs than
// it stamped, would satisfy every uniqueness assertion above.
func TestJobsSQLClaim_EveryReadyJobIsEventuallyClaimedExactlyOnce(t *testing.T) {
	db := rawJobsDB(t)
	const total = 50
	q := seedJobs(t, db, "claim_all_jobs", total)
	ctx := context.Background()

	seen := map[string]bool{}
	for range total * 2 {
		got, err := q.Dequeue(ctx, 3)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		if len(got) == 0 {
			break
		}
		for _, j := range got {
			seen[j.ID] = true
		}
	}

	if len(seen) != total {
		t.Errorf("claimed %d distinct jobs, want all %d: jobs were stranded", len(seen), total)
	}
}

// The quiet half of JB-1, and the one that loses work rather than duplicating
// it. The old UPDATE stamped every row it matched — status running, attempts
// incremented — but the follow-up SELECT was capped at LIMIT n and ordered, so
// once several claims shared a lease string it kept returning the same top rows
// and the rest were stamped but handed to nobody. Those jobs sat for the full
// lease having already spent an attempt without executing, and enough rounds of
// that exhausts max_retry and dead-letters a job that never ran once.
//
// The invariant: a job's attempts counter moves only when a caller receives it.
func TestJobsSQLClaim_NoJobIsStampedWithoutBeingDelivered(t *testing.T) {
	db := rawJobsDB(t)
	const total = 30
	q := seedJobs(t, db, "claim_strand_jobs", total)
	ctx := context.Background()

	delivered := map[string]int{}
	for range 10 {
		got, err := q.Dequeue(ctx, 2)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		for _, j := range got {
			delivered[j.ID]++
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT id, status, attempts FROM claim_strand_jobs`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()

	var stranded int
	for rows.Next() {
		var id, status string
		var attempts int
		if err := rows.Scan(&id, &status, &attempts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if status == "running" && delivered[id] == 0 {
			stranded++
		}
		if attempts != delivered[id] {
			t.Errorf("job %s: attempts=%d but delivered %d times", id, attempts, delivered[id])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if stranded > 0 {
		t.Errorf("%d job(s) marked running that no caller ever received: "+
			"they hold a lease and have burned an attempt without executing", stranded)
	}
}

// A claimed job must actually be marked running with a live lease, or a second
// pass picks it straight back up regardless of how the rows are returned.
func TestJobsSQLClaim_ClaimedRowsAreLeased(t *testing.T) {
	db := rawJobsDB(t)
	q := seedJobs(t, db, "claim_lease_jobs", 5)
	ctx := context.Background()

	got, err := q.Dequeue(ctx, 3)
	if err != nil || len(got) != 3 {
		t.Fatalf("dequeue: got %d jobs, err %v", len(got), err)
	}

	for _, j := range got {
		var status, lease string
		err := db.QueryRowContext(ctx,
			`SELECT status, COALESCE(lease_until,'') FROM claim_lease_jobs WHERE id=?`, j.ID,
		).Scan(&status, &lease)
		if err != nil {
			t.Fatalf("read back %s: %v", j.ID, err)
		}
		if status != "running" {
			t.Errorf("job %s status = %q, want running", j.ID, status)
		}
		if lease == "" {
			t.Errorf("job %s has no lease: nothing stops another worker taking it", j.ID)
		}
		if parsed, perr := time.Parse(time.RFC3339Nano, lease); perr == nil && !parsed.After(time.Now()) {
			t.Errorf("job %s lease %v is already expired", j.ID, parsed)
		}
	}
}
