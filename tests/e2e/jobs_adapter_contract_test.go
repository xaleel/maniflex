package e2e

// A conformance suite for the queue adapters, driven entirely through the
// exported interfaces — jobs.Queue, jobs.Source, jobs.Cancellable,
// jobs.Inspector, jobs.Requeuer — with no reach into either package's
// internals. The Worker only ever sees an adapter through these, so this is the
// surface that has to hold; anything asserted here is a promise a third-party
// adapter must keep too.
//
// Where the two adapters legitimately differ, the assertion is made on the
// shared contract rather than on one implementation's mechanics. The clearest
// case is a finished job: jobs/sql keeps the row as 'succeeded' while
// jobs/inproc drops the entry outright, so the contract both keep is "it is no
// longer claimable", and that is what these tests check.
//
//	go test ./e2e/ -run TestAdapterContract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
	"github.com/xaleel/maniflex/jobs/inproc"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
)

// errNack stands in for whatever the handler returned.
var errNack = errors.New("handler failed")

// queueAdapter is every optional capability the built-in adapters advertise.
// Compile-time proof that both still satisfy the whole set is itself part of
// the contract: dropping one silently would strip the Worker of cancellation,
// introspection or budget-free requeue with nothing to notice.
type queueAdapter interface {
	jobs.Queue
	jobs.Source
	jobs.Cancellable
	jobs.Inspector
	jobs.Requeuer
}

var (
	_ queueAdapter = (*inproc.Queue)(nil)
	_ queueAdapter = (*jobssql.Queue)(nil)
)

type adapterCase struct {
	name string
	open func(t *testing.T) queueAdapter
}

func adapterCases() []adapterCase {
	return []adapterCase{
		{
			name: "inproc",
			open: func(*testing.T) queueAdapter { return inproc.New() },
		},
		{
			name: "sql",
			open: func(t *testing.T) queueAdapter {
				t.Helper()
				db := rawJobsDB(t) // its own isolated in-memory server per test
				if err := jobssql.Migrate(context.Background(), db, jobsDriver()); err != nil {
					t.Fatalf("migrate: %v", err)
				}
				return jobssql.New(db)
			},
		},
	}
}

// forEachAdapter runs fn as a subtest against every adapter.
func forEachAdapter(t *testing.T, fn func(t *testing.T, q queueAdapter)) {
	t.Helper()
	for _, c := range adapterCases() {
		t.Run(c.name, func(t *testing.T) { fn(t, c.open(t)) })
	}
}

// claimAll drains everything currently ready, so a test can ask "what is
// claimable right now" without assuming a batch size.
func claimAll(t *testing.T, q queueAdapter) []jobs.Job {
	t.Helper()
	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	return got
}

func mustEnqueue(t *testing.T, q queueAdapter, j jobs.Job) string {
	t.Helper()
	id, err := q.Enqueue(context.Background(), j)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// ── scheduling ────────────────────────────────────────────────────────────────

// EnqueueAt is the delayed-execution seam: nothing may run before its time.
func TestAdapterContract_EnqueueAtWithholdsUntilDue(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id, err := q.EnqueueAt(ctx, jobs.Job{Type: "later"}, time.Now().Add(80*time.Millisecond))
		if err != nil {
			t.Fatalf("enqueue at: %v", err)
		}

		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("claimed %d jobs before the scheduled time", len(got))
		}

		time.Sleep(150 * time.Millisecond)

		got := claimAll(t, q)
		if len(got) != 1 || got[0].ID != id {
			t.Fatalf("after the delay claimed %d jobs, want the one scheduled", len(got))
		}
	})
}

// A time already past means "now", not "never".
func TestAdapterContract_EnqueueAtInThePastIsImmediate(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		if _, err := q.EnqueueAt(context.Background(), jobs.Job{Type: "overdue"}, time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("enqueue at: %v", err)
		}
		if got := claimAll(t, q); len(got) != 1 {
			t.Fatalf("claimed %d jobs, want the overdue one", len(got))
		}
	})
}

func TestAdapterContract_EnqueueBatchReturnsOneIDPerJob(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		in := []jobs.Job{{Type: "batch"}, {Type: "batch"}, {Type: "batch"}}
		ids, err := q.EnqueueBatch(context.Background(), in)
		if err != nil {
			t.Fatalf("enqueue batch: %v", err)
		}
		if len(ids) != len(in) {
			t.Fatalf("got %d ids for %d jobs", len(ids), len(in))
		}
		seen := map[string]bool{}
		for i, id := range ids {
			if id == "" {
				t.Fatalf("id %d is empty", i)
			}
			if seen[id] {
				t.Fatalf("id %q was issued twice", id)
			}
			seen[id] = true
		}
		if got := claimAll(t, q); len(got) != len(in) {
			t.Fatalf("claimed %d of %d batched jobs", len(got), len(in))
		}
	})
}

// The caller may assign the id; the adapter must honour it rather than
// overwrite it, since the caller has already recorded it elsewhere.
func TestAdapterContract_CallerSuppliedIDIsKept(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		if got := mustEnqueue(t, q, jobs.Job{ID: "chosen-id", Type: "t"}); got != "chosen-id" {
			t.Fatalf("Enqueue returned %q for a caller-assigned id", got)
		}
		claimed := claimAll(t, q)
		if len(claimed) != 1 || claimed[0].ID != "chosen-id" {
			t.Fatalf("claimed %v, want the caller's id", claimed)
		}
	})
}

// ── delivery and settlement ───────────────────────────────────────────────────

// Attempts is what the Worker's retry budget is measured against, so a delivery
// has to stamp it — and the second delivery has to differ from the first.
func TestAdapterContract_DeliveryStampsAttempts(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t", MaxRetry: 5})

		first := claimAll(t, q)
		if len(first) != 1 || first[0].Attempts != 1 {
			t.Fatalf("first delivery: %d jobs, attempts=%v, want 1 job at attempt 1", len(first), first)
		}
		if err := q.Nack(ctx, id, errNack, time.Millisecond); err != nil {
			t.Fatalf("nack: %v", err)
		}
		time.Sleep(60 * time.Millisecond)

		second := claimAll(t, q)
		if len(second) != 1 || second[0].Attempts != 2 {
			t.Fatalf("second delivery: %d jobs, %+v, want 1 job at attempt 2", len(second), second)
		}
	})
}

// Nack's delay is the retry backoff the Worker computes; holding it is what
// stops a failing job from spinning.
func TestAdapterContract_NackHonoursItsDelay(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t", MaxRetry: 5})
		claimAll(t, q)

		if err := q.Nack(ctx, id, errNack, 120*time.Millisecond); err != nil {
			t.Fatalf("nack: %v", err)
		}
		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("claimed %d jobs during the retry delay", len(got))
		}
		time.Sleep(200 * time.Millisecond)
		if got := claimAll(t, q); len(got) != 1 {
			t.Fatalf("claimed %d jobs after the delay elapsed, want 1", len(got))
		}
	})
}

// A Nack past the budget retires the job. Both adapters make it unclaimable;
// only how they record it differs.
func TestAdapterContract_NackAtBudgetRetiresTheJob(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t", MaxRetry: 1})
		claimAll(t, q) // attempts → 1, equal to MaxRetry

		if err := q.Nack(ctx, id, errNack, time.Millisecond); err != nil {
			t.Fatalf("nack: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("claimed %d jobs after the budget was spent — it would retry forever", len(got))
		}
	})
}

func TestAdapterContract_AckedJobIsNotClaimableAgain(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t"})
		claimAll(t, q)

		if err := q.Ack(ctx, id); err != nil {
			t.Fatalf("ack: %v", err)
		}
		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("a succeeded job was handed out again (%d jobs)", len(got))
		}
	})
}

func TestAdapterContract_DeadJobIsNotClaimableAgain(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t", MaxRetry: 5})
		claimAll(t, q)

		if err := q.Dead(ctx, id, errNack); err != nil {
			t.Fatalf("dead: %v", err)
		}
		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("a dead-lettered job was handed out again (%d jobs)", len(got))
		}
	})
}

// Requeue is the budget-free return path the Worker uses for a type it cannot
// handle: the job comes back with the attempt count it was given, so shuttling
// it past a worker does not erode the budget its real handler will need.
func TestAdapterContract_RequeueRestoresWithoutSpendingBudget(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		mustEnqueue(t, q, jobs.Job{Type: "t", MaxRetry: 5})

		claimed := claimAll(t, q)
		if len(claimed) != 1 {
			t.Fatalf("claimed %d jobs", len(claimed))
		}
		j := claimed[0]
		j.Attempts-- // what the Worker does before requeuing
		if err := q.Requeue(ctx, j, time.Millisecond); err != nil {
			t.Fatalf("requeue: %v", err)
		}
		time.Sleep(60 * time.Millisecond)

		again := claimAll(t, q)
		if len(again) != 1 {
			t.Fatalf("claimed %d jobs after requeue, want 1", len(again))
		}
		if again[0].Attempts != 1 {
			t.Fatalf("attempts = %d after a requeue round-trip, want 1 — the budget was spent", again[0].Attempts)
		}
	})
}

// ── Cancellable ───────────────────────────────────────────────────────────────

func TestAdapterContract_CancelStopsAnEnqueuedJob(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t"})

		if err := q.Cancel(ctx, id); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := claimAll(t, q); len(got) != 0 {
			t.Fatalf("a cancelled job was still claimed (%d jobs)", len(got))
		}
	})
}

// Cancelling work already in flight must fail rather than silently succeed:
// the handler is running and the caller has to know it was not stopped.
func TestAdapterContract_CancelRefusesARunningJob(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "t"})
		claimAll(t, q)

		if err := q.Cancel(ctx, id); err == nil {
			t.Fatal("cancelling a running job reported success")
		}
	})
}

func TestAdapterContract_CancelUnknownIDErrors(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		if err := q.Cancel(context.Background(), "no-such-job"); err == nil {
			t.Fatal("cancelling an unknown id reported success")
		}
	})
}

// ── Inspector ─────────────────────────────────────────────────────────────────

func TestAdapterContract_GetReportsTheJobAndStatus(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		id := mustEnqueue(t, q, jobs.Job{Type: "report", ActorID: "u1", TenantID: "t1"})

		st, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if st.Status != jobs.StatusEnqueued {
			t.Errorf("status = %q, want %q", st.Status, jobs.StatusEnqueued)
		}
		if st.Job.Type != "report" || st.Job.ActorID != "u1" || st.Job.TenantID != "t1" {
			t.Errorf("Get lost job fields: %+v", st.Job)
		}

		claimAll(t, q)
		running, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("get after claim: %v", err)
		}
		if running.Status != jobs.StatusRunning {
			t.Errorf("status after claim = %q, want %q", running.Status, jobs.StatusRunning)
		}
	})
}

func TestAdapterContract_GetUnknownIDErrors(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		if _, err := q.Get(context.Background(), "no-such-job"); err == nil {
			t.Fatal("Get on an unknown id reported success")
		}
	})
}

// List backs the /job_statuses-style views, so its filters have to actually
// filter — an ignored ActorID would leak one caller's jobs to another.
func TestAdapterContract_ListFiltersByField(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		mustEnqueue(t, q, jobs.Job{Type: "alpha", ActorID: "u1", TenantID: "t1"})
		mustEnqueue(t, q, jobs.Job{Type: "alpha", ActorID: "u2", TenantID: "t2"})
		mustEnqueue(t, q, jobs.Job{Type: "beta", ActorID: "u1", TenantID: "t1"})

		for _, tc := range []struct {
			name  string
			query jobs.ListQuery
			want  int
		}{
			{"by type", jobs.ListQuery{Type: "alpha"}, 2},
			{"by actor", jobs.ListQuery{ActorID: "u1"}, 2},
			{"by tenant", jobs.ListQuery{TenantID: "t2"}, 1},
			{"by type and actor", jobs.ListQuery{Type: "alpha", ActorID: "u1"}, 1},
			{"by status", jobs.ListQuery{Status: jobs.StatusEnqueued}, 3},
			{"no match", jobs.ListQuery{ActorID: "nobody"}, 0},
			{"unfiltered", jobs.ListQuery{}, 3},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := q.List(ctx, tc.query)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(got) != tc.want {
					t.Fatalf("got %d rows, want %d (query %+v)", len(got), tc.want, tc.query)
				}
			})
		}
	})
}

func TestAdapterContract_ListHonoursLimit(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		for range 5 {
			mustEnqueue(t, q, jobs.Job{Type: "many"})
		}
		got, err := q.List(ctx, jobs.ListQuery{Limit: 2})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d rows for Limit=2", len(got))
		}
	})
}

// ── lifecycle ─────────────────────────────────────────────────────────────────

// After Close the queue must refuse work rather than accept it into a void.
func TestAdapterContract_ClosedQueueRefusesWork(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, q queueAdapter) {
		ctx := context.Background()
		if err := q.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := q.Enqueue(ctx, jobs.Job{Type: "t"}); err == nil {
			t.Fatal("Enqueue succeeded on a closed queue")
		}
		if _, err := q.Dequeue(ctx, 1); err == nil {
			t.Fatal("Dequeue succeeded on a closed queue")
		}
	})
}
