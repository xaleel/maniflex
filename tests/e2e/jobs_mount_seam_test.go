package e2e

// The queue wrapper jobsmaniflex.Mount returns. Applications hold it as a plain
// jobs.Queue and hand the same value to WorkerConfig.Source, so every method has
// to both do the queue's job and keep the status row in step. These tests drive
// it through the exported surface only — Mount, the returned jobs.Queue, and the
// REST view of the status rows.
//
//	go test ./e2e/ -run TestMountSeam

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/jobs"
	jobsmaniflex "github.com/xaleel/maniflex/jobs/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Every enqueue entry point must create the status row, not just Enqueue — a
// client that polls /job_statuses/:id right after an EnqueueAt would otherwise
// get a 404 for a job that exists.
func TestMountSeam_EnqueueAtCreatesTheStatusRow(t *testing.T) {
	t.Parallel()
	srv, _, q := newJobsServer(t)

	id, err := q.EnqueueAt(context.Background(),
		jobs.Job{Type: "delayed_report", ActorID: "user-7"},
		time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("EnqueueAt: %v", err)
	}

	resp := srv.GET("/job_statuses/" + id)
	resp.AssertStatus(http.StatusOK)
	data := resp.Data()
	if data["status"] != string(jobs.StatusEnqueued) {
		t.Errorf("status = %v, want %q", data["status"], jobs.StatusEnqueued)
	}
	if data["type"] != "delayed_report" {
		t.Errorf("type = %v, want %q", data["type"], "delayed_report")
	}
	if data["actor_id"] != "user-7" {
		t.Errorf("actor_id = %v, want %q", data["actor_id"], "user-7")
	}
}

func TestMountSeam_EnqueueBatchCreatesARowPerJob(t *testing.T) {
	t.Parallel()
	srv, _, q := newJobsServer(t)

	in := []jobs.Job{
		{Type: "batch_a", ActorID: "user-1"},
		{Type: "batch_b", ActorID: "user-2"},
		{Type: "batch_c", ActorID: "user-3"},
	}
	ids, err := q.EnqueueBatch(context.Background(), in)
	if err != nil {
		t.Fatalf("EnqueueBatch: %v", err)
	}
	if len(ids) != len(in) {
		t.Fatalf("got %d ids for %d jobs", len(ids), len(in))
	}

	// Each row must carry its own job's fields — a loop that indexed the wrong
	// slice would still produce three rows, all describing the same job.
	for i, id := range ids {
		resp := srv.GET("/job_statuses/" + id)
		resp.AssertStatus(http.StatusOK)
		data := resp.Data()
		if data["type"] != in[i].Type {
			t.Errorf("row %d: type = %v, want %q", i, data["type"], in[i].Type)
		}
		if data["actor_id"] != in[i].ActorID {
			t.Errorf("row %d: actor_id = %v, want %q", i, data["actor_id"], in[i].ActorID)
		}
	}
}

// ── delegation to the inner queue ─────────────────────────────────────────────

// recordingInner is an inner queue that records what the wrapper forwarded, so
// the delegation can be observed without reaching into the wrapper.
type recordingInner struct {
	mu sync.Mutex

	enqueued  []jobs.Job
	nacked    []string
	nackDelay time.Duration
	closed    bool
	blocked   int

	// blockingSupported toggles whether this value also satisfies
	// jobs.BlockingSource, which is what decides the wrapper Mount hands back.
	pending []jobs.Job
}

func (q *recordingInner) Enqueue(_ context.Context, j jobs.Job) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j.ID == "" {
		j.ID = "inner-1"
	}
	q.enqueued = append(q.enqueued, j)
	return j.ID, nil
}

func (q *recordingInner) EnqueueAt(ctx context.Context, j jobs.Job, _ time.Time) (string, error) {
	return q.Enqueue(ctx, j)
}

func (q *recordingInner) EnqueueBatch(ctx context.Context, js []jobs.Job) ([]string, error) {
	ids := make([]string, len(js))
	for i, j := range js {
		id, err := q.Enqueue(ctx, j)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (q *recordingInner) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	return nil
}

func (q *recordingInner) Dequeue(context.Context, int) ([]jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.pending
	q.pending = nil
	return out, nil
}

func (q *recordingInner) Ack(context.Context, string) error { return nil }

func (q *recordingInner) Nack(_ context.Context, id string, _ error, d time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nacked = append(q.nacked, id)
	q.nackDelay = d
	return nil
}

func (q *recordingInner) Dead(context.Context, string, error) error { return nil }

func (q *recordingInner) Cancel(context.Context, string) error { return nil }

// blockingInner adds jobs.BlockingSource, which is what makes Mount return the
// blocking wrapper instead of the plain one.
type blockingInner struct{ *recordingInner }

func (q *blockingInner) DequeueBlocking(ctx context.Context, n int, _ time.Duration) ([]jobs.Job, error) {
	q.mu.Lock()
	q.blocked++
	q.mu.Unlock()
	return q.Dequeue(ctx, n)
}

// mountInner mounts inner on a throwaway server and returns the wrapped queue.
func mountInner(t *testing.T, inner jobs.Queue) jobs.Queue {
	t.Helper()
	var wrapped jobs.Queue
	testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			_, q, err := jobsmaniflex.Mount(s, inner)
			if err != nil {
				t.Fatalf("Mount: %v", err)
			}
			wrapped = q
		},
	})
	return wrapped
}

// Nack is on the Source half of the wrapper: the Worker calls it to schedule a
// retry, and the delay it computed has to survive the hop.
func TestMountSeam_NackReachesTheInnerQueue(t *testing.T) {
	t.Parallel()
	inner := &recordingInner{}
	wrapped := mountInner(t, inner)

	src, ok := wrapped.(jobs.Source)
	if !ok {
		t.Fatal("the mounted queue does not implement jobs.Source; a Worker could not use it")
	}
	if err := src.Nack(context.Background(), "job-1", errors.New("boom"), 42*time.Second); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.nacked) != 1 || inner.nacked[0] != "job-1" {
		t.Fatalf("inner queue saw nacks %v, want [job-1]", inner.nacked)
	}
	if inner.nackDelay != 42*time.Second {
		t.Errorf("inner queue saw delay %v, want the 42s the caller asked for", inner.nackDelay)
	}
}

// Close has to reach the inner queue, or an application that closes the value
// Mount handed back leaks the real queue's resources.
func TestMountSeam_CloseReachesTheInnerQueue(t *testing.T) {
	t.Parallel()
	inner := &recordingInner{}
	wrapped := mountInner(t, inner)

	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if !inner.closed {
		t.Fatal("closing the mounted queue did not close the inner queue")
	}
}

// When the inner queue can long-poll, the wrapper must advertise it too —
// otherwise mounting silently downgrades a Redis-backed queue to busy polling.
func TestMountSeam_BlockingSourceSurvivesTheWrapper(t *testing.T) {
	t.Parallel()
	inner := &blockingInner{recordingInner: &recordingInner{
		pending: []jobs.Job{{ID: "j1", Type: "t"}},
	}}
	wrapped := mountInner(t, inner)

	bs, ok := wrapped.(jobs.BlockingSource)
	if !ok {
		t.Fatal("the mounted queue lost jobs.BlockingSource; the Worker would fall back to polling")
	}
	got, err := bs.DequeueBlocking(context.Background(), 1, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("DequeueBlocking: %v", err)
	}
	if len(got) != 1 || got[0].ID != "j1" {
		t.Fatalf("DequeueBlocking returned %v, want the inner queue's job", got)
	}

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if inner.blocked != 1 {
		t.Errorf("inner DequeueBlocking called %d times, want 1", inner.blocked)
	}
}

// The converse: a queue with no long-poll must not be dressed up as having one,
// or the Worker would call a method the inner queue cannot service.
func TestMountSeam_NonBlockingInnerIsNotAdvertisedAsBlocking(t *testing.T) {
	t.Parallel()
	wrapped := mountInner(t, &recordingInner{})

	if _, ok := wrapped.(jobs.BlockingSource); ok {
		t.Fatal("the wrapper claims jobs.BlockingSource for an inner queue that has none")
	}
}
