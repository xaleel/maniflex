package e2e

// Postgres-only behaviour of jobs/sql. These assert things SQLite structurally
// cannot exercise: the dialect choice (JB-6), and the concurrency guarantees
// that exist because two Postgres claims can genuinely overlap — SQLite's
// database-level write lock serialises whole claims, so its version of these
// tests proves something weaker.
//
//	MANIFLEX_TEST_DB=postgres MANIFLEX_TEST_PG_DSN=... go test ./e2e/ -run TestJobsPG

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func skipUnlessPostgres(t *testing.T) {
	t.Helper()
	if !testutil.IsPostgres() {
		t.Skip("Postgres-lane test: run with MANIFLEX_TEST_DB=postgres")
	}
}

// ── dialect selection (audit JB-6) ────────────────────────────────────────────

// The driver here is lib/pq. Auto-detection has to classify it as Postgres, or
// the adapter emits SQLite SQL with "?" placeholders against a Postgres server
// and every statement fails.
func TestJobsPG_DialectIsAutoDetected(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName("pg_detect")); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q := jobssql.New(db, jobssql.WithTableName("pg_detect")) // no WithDriver: detect
	id, err := q.Enqueue(ctx, jobs.Job{Type: "detect"})
	if err != nil {
		t.Fatalf("enqueue with an auto-detected dialect: %v", err)
	}
	got, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("dequeue with an auto-detected dialect: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("claimed %v, want the enqueued job — detection produced the wrong dialect", got)
	}
}

// WithDriver("postgres") states explicitly what detection would have inferred,
// for drivers the package does not recognise.
func TestJobsPG_WithDriverForcesPostgres(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName("pg_forced")); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q := jobssql.New(db,
		jobssql.WithTableName("pg_forced"),
		jobssql.WithDriver("postgres"),
	)
	if _, err := q.Enqueue(ctx, jobs.Job{Type: "forced"}); err != nil {
		t.Fatalf("enqueue with WithDriver(postgres): %v", err)
	}
	got, err := q.Dequeue(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("dequeue with WithDriver(postgres): err=%v n=%d", err, len(got))
	}
}

// The other half of JB-6's promise: a wrong dialect does not silently degrade,
// it fails outright. Forcing SQLite against Postgres sends "?" placeholders,
// which Postgres rejects — loudly, which is the point.
func TestJobsPG_WrongDriverFailsLoudly(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName("pg_wrong")); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q := jobssql.New(db,
		jobssql.WithTableName("pg_wrong"),
		jobssql.WithDriver("sqlite"), // deliberately wrong for this server
	)
	if _, err := q.Enqueue(ctx, jobs.Job{Type: "wrong"}); err == nil {
		t.Fatal("enqueue succeeded with the SQLite dialect forced against Postgres; " +
			"a wrong dialect must fail rather than appear to work")
	}
}

// ── concurrency (the reason the partial unique index exists) ──────────────────

// Two Postgres claims can overlap: each snapshots before the other commits, so
// neither sees the other's rows. FOR UPDATE SKIP LOCKED is what keeps them off
// each other's candidates, and the guarantee is that no job is handed to two
// callers. SQLite cannot test this — its write lock serialises whole claims.
func TestJobsPG_ConcurrentClaimsNeverDeliverAJobTwice(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	const table = "pg_concurrent"
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table))

	const total = 60
	for i := range total {
		if _, err := q.Enqueue(ctx, jobs.Job{
			Type:    "pg.concurrent",
			Payload: fmt.Appendf(nil, `{"i":%d}`, i),
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var (
		mu         sync.Mutex
		deliveries = map[string]int{}
	)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for {
				got, err := q.Dequeue(ctx, 5)
				if err != nil {
					t.Errorf("dequeue: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, j := range got {
					deliveries[j.ID]++
				}
				mu.Unlock()
				for _, j := range got {
					if err := q.Ack(ctx, j.ID); err != nil {
						t.Errorf("ack: %v", err)
						return
					}
				}
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	var doubled []string
	for id, n := range deliveries {
		if n > 1 {
			doubled = append(doubled, fmt.Sprintf("%s×%d", id, n))
		}
	}
	if len(doubled) > 0 {
		t.Fatalf("%d jobs were delivered more than once: %v — SKIP LOCKED is not isolating claimers",
			len(doubled), doubled)
	}
	if len(deliveries) != total {
		t.Errorf("delivered %d distinct jobs, want %d — some were never claimed", len(deliveries), total)
	}
}

// The GroupKey invariant under real overlap: at most one job per key may be in
// 'running' at any moment. On Postgres this is held by the partial unique index,
// not by the claim's WHERE clause, because two claimers cannot see each other's
// uncommitted work.
func TestJobsPG_GroupKeyNeverRunsTwiceAtOnce(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	const table = "pg_groupkey"
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName(table))

	keys := []string{"tenant-a", "tenant-b", "tenant-c"}
	for _, k := range keys {
		for range 8 {
			if _, err := q.Enqueue(ctx, jobs.Job{Type: "pg.group", GroupKey: k}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
	}

	var (
		mu       sync.Mutex
		live     = map[string]int{}
		violated string
	)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for {
				got, err := q.Dequeue(ctx, 4)
				if err != nil {
					// A group-running index violation that survived the adapter's
					// retry loop is a robustness defect, not a safety one: the
					// invariant under test still held. Recorded, not fatal, so
					// this test reports on isolation rather than on retry depth.
					t.Logf("dequeue returned %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, j := range got {
					live[j.GroupKey]++
					if live[j.GroupKey] > 1 {
						violated = fmt.Sprintf("key %q had %d jobs running at once", j.GroupKey, live[j.GroupKey])
					}
				}
				mu.Unlock()

				time.Sleep(2 * time.Millisecond) // hold the claim so overlap is real

				mu.Lock()
				for _, j := range got {
					live[j.GroupKey]--
				}
				mu.Unlock()
				for _, j := range got {
					if err := q.Ack(ctx, j.ID); err != nil {
						t.Errorf("ack: %v", err)
						return
					}
				}
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if violated != "" {
		t.Fatalf("GroupKey serialisation broken: %s", violated)
	}
}

// The reclaim sweep (NEW-2) is dialect-specific SQL — CASE over COALESCE/NULLIF
// with a Postgres timestamp comparison. Confirm it does on Postgres what the
// SQLite lane already proves.
func TestJobsPG_ExpiredLeaseIsReclaimed(t *testing.T) {
	skipUnlessPostgres(t)
	ctx := context.Background()
	db := rawJobsDB(t)
	const table = "pg_reclaim"
	if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db,
		jobssql.WithTableName(table),
		jobssql.WithLeaseDuration(50*time.Millisecond),
	)

	id, err := q.Enqueue(ctx, jobs.Job{Type: "pg.reclaim", MaxRetry: 5})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got, err := q.Dequeue(ctx, 1); err != nil || len(got) != 1 {
		t.Fatalf("claim: err=%v n=%d", err, len(got))
	}
	// The worker dies here.

	time.Sleep(150 * time.Millisecond)

	again, err := q.Dequeue(ctx, 1)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 1 || again[0].ID != id {
		t.Fatalf("the crashed worker's job was not reclaimed on Postgres (got %d jobs)", len(again))
	}
	if again[0].Attempts != 2 {
		t.Errorf("attempts = %d after reclaim, want 2", again[0].Attempts)
	}
}

// ── NEW-3: colliding claims must resolve inside the adapter ───────────────────

// contendedOutRecorder captures the one debug record jobs/sql emits when a claim
// loses the group-key race on every attempt. That record is the only precise
// signal available: exhaustion returns "no jobs" rather than an error, so a
// caller cannot tell it from an empty queue, and counting returned errors would
// silently stop detecting a regression of the retry jitter.
type contendedOutRecorder struct {
	mu    sync.Mutex
	table string
	hits  int
}

func (r *contendedOutRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *contendedOutRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *contendedOutRecorder) WithGroup(string) slog.Handler            { return r }

func (r *contendedOutRecorder) Handle(_ context.Context, rec slog.Record) error {
	if !strings.Contains(rec.Message, "claim contended out") {
		return nil
	}
	// Only count this test's tables: the e2e package runs tests in parallel and
	// the default logger is process-wide.
	var mine bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "table" && strings.HasPrefix(a.Value.String(), r.table) {
			mine = true
		}
		return true
	})
	if !mine {
		return nil
	}
	r.mu.Lock()
	r.hits++
	r.mu.Unlock()
	return nil
}

func (r *contendedOutRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// Two Postgres claimers can both see a group key as free — neither's UPDATE has
// committed — pick the same job and both write it, so the partial unique index
// rejects the loser. The adapter is meant to absorb that by retrying. With only
// three attempts it ran out often enough that the raw "duplicate key value
// violates unique constraint" surfaced to the caller in about 40% of runs at
// this concurrency, stalling a worker for a second over ordinary contention
// (audit NEW-3).
//
// What fixed it was the attempt count. Losing is structural here: with 24
// claimers over 2 keys most are contending at any instant, so a claimer's odds
// do not improve by waiting, only by trying again. Pacing the retries was
// measured and made no difference — see the note on maxClaimRetries.
//
// One pass reproduced the old behaviour ~40% of the time, so the guard runs
// several and requires every one to resolve inside the adapter. Measured:
// bound=3 gives 3-7 exhaustions per 12 passes and never zero; bound=8 gives
// zero across 90.
func TestJobsPG_CollidingClaimsResolveWithoutContendingOut(t *testing.T) {
	skipUnlessPostgres(t)

	const (
		// Mirrors jobs/sql's maxClaimRetries; used only in the failure message.
		maxAttemptsDocumented = 8
		tablePrefix           = "pg_collide"
		iterations            = 12
		workers               = 24
		keys                  = 2
		perKey                = 20
	)

	rec := &contendedOutRecorder{table: tablePrefix}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	db := rawJobsDB(t)

	for iter := range iterations {
		table := fmt.Sprintf("%s_%d", tablePrefix, iter)
		if err := jobssql.Migrate(ctx, db, "postgres", jobssql.WithTableName(table)); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		q := jobssql.New(db, jobssql.WithTableName(table))

		total := keys * perKey
		for k := range keys {
			for range perKey {
				if _, err := q.Enqueue(ctx, jobs.Job{
					Type: "pg.collide", GroupKey: fmt.Sprintf("key-%d", k),
				}); err != nil {
					t.Fatalf("enqueue: %v", err)
				}
			}
		}

		var done atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for {
					got, err := q.Dequeue(ctx, 1)
					if err != nil {
						t.Errorf("dequeue: %v", err)
						return
					}
					if len(got) == 0 {
						return
					}
					for _, j := range got {
						if err := q.Ack(ctx, j.ID); err != nil {
							t.Errorf("ack: %v", err)
							return
						}
						done.Add(1)
					}
				}
			})
		}
		wg.Wait()

		// Absorbing a collision must not cost the job: whatever a claimer skips
		// stays claimable for the next one.
		if got := done.Load(); got != int64(total) {
			t.Errorf("iteration %d: completed %d of %d jobs", iter, got, total)
		}
	}

	if n := rec.count(); n != 0 {
		t.Fatalf("%d claims gave up after %d attempts across %d passes; the retry bound is too low "+
			"for this contention and callers get empty results instead of the work",
			n, maxAttemptsDocumented, iterations)
	}
}
