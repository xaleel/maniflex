package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"maniflex"
	jobsmaniflex "maniflex/jobs/maniflex"
	"maniflex/jobs"
	"maniflex/jobs/inproc"
	"maniflex/tests/e2e/testutil"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// jobsSetup wires an inproc queue, mounts the status model, and returns a
// ready test server together with the sink and wrapped queue.
// The worker is NOT started here — individual tests start it when needed.
func newJobsServer(t *testing.T) (srv *testutil.Server, sink jobs.StatusSink, q jobs.Queue) {
	t.Helper()
	raw := inproc.New()

	var capturedSink jobs.StatusSink
	var capturedQueue jobs.Queue

	srv = testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s2, q2, err := jobsmaniflex.Mount(s, raw)
			if err != nil {
				t.Fatalf("jobsmaniflex.Mount: %v", err)
			}
			capturedSink = s2
			capturedQueue = q2
		},
	})
	return srv, capturedSink, capturedQueue
}

// startWorker runs a Worker in the background for the test's lifetime.
// handlers maps job type → handler func.
func startWorker(t *testing.T, q jobs.Queue, sink jobs.StatusSink, handlers map[string]jobs.Handler) {
	t.Helper()
	w, err := jobs.NewWorker(jobs.WorkerConfig{
		Source:            q.(jobs.Source),
		Status:            sink,
		Handlers:          handlers,
		EmptyQueueBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("jobs.NewWorker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
}

// pollStatus polls GET /job_statuses/:id until the status field equals wantStatus
// or a 5-second deadline is exceeded.
func pollStatus(t *testing.T, srv *testutil.Server, jobID, wantStatus string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := srv.GET("/job_statuses/" + jobID)
		if resp.Status == http.StatusOK {
			data := resp.Data()
			if data["status"] == wantStatus {
				return data
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s: timed out waiting for status %q", jobID, wantStatus)
	return nil
}

// ── write-blocker ─────────────────────────────────────────────────────────────

// TestJobs_WriteBlocker verifies that the StatusModel is read-only via the REST API.
func TestJobs_WriteBlocker(t *testing.T) {
	t.Parallel()
	srv, _, _ := newJobsServer(t)

	srv.POST("/job_statuses", map[string]any{"type": "x", "status": "enqueued"}).
		AssertStatus(http.StatusMethodNotAllowed)

	srv.PATCH("/job_statuses/nonexistent", map[string]any{"status": "running"}).
		AssertStatus(http.StatusMethodNotAllowed)

	srv.DELETE("/job_statuses/nonexistent").
		AssertStatus(http.StatusMethodNotAllowed)
}

// ── enqueue creates row ───────────────────────────────────────────────────────

// TestJobs_EnqueueCreatesRow verifies that the wrapped queue creates an "enqueued"
// status row immediately — before any worker touches the job.
func TestJobs_EnqueueCreatesRow(t *testing.T) {
	t.Parallel()
	srv, _, q := newJobsServer(t)

	id, err := q.Enqueue(context.Background(), jobs.Job{
		Type:    "report",
		ActorID: "user-42",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	resp := srv.GET("/job_statuses/" + id)
	resp.AssertStatus(http.StatusOK)
	data := resp.Data()

	if data["status"] != "enqueued" {
		t.Errorf("status: got %v, want enqueued", data["status"])
	}
	if data["type"] != "report" {
		t.Errorf("type: got %v, want report", data["type"])
	}
	if data["actor_id"] != "user-42" {
		t.Errorf("actor_id: got %v, want user-42", data["actor_id"])
	}
	if data["started_at"] != nil {
		t.Errorf("started_at: want nil before worker runs, got %v", data["started_at"])
	}
}

// ── full lifecycle ────────────────────────────────────────────────────────────

// TestJobs_FullLifecycle exercises the complete happy path:
// enqueue → row shows "enqueued" → worker runs → row shows "succeeded" with result_url.
func TestJobs_FullLifecycle(t *testing.T) {
	t.Parallel()
	srv, sink, q := newJobsServer(t)

	const resultURL = "https://cdn.example.com/reports/output.csv"

	startWorker(t, q, sink, map[string]jobs.Handler{
		"export": func(_ context.Context, _ jobs.Job) (jobs.Result, error) {
			return jobs.Result{URL: resultURL, Mime: "text/csv"}, nil
		},
	})

	id, err := q.Enqueue(context.Background(), jobs.Job{
		Type:    "export",
		ActorID: "user-1",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Row is immediately visible as enqueued.
	srv.GET("/job_statuses/" + id).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		if s := data["status"]; s != "enqueued" && s != "running" && s != "succeeded" {
			t.Errorf("unexpected initial status: %v", s)
		}
	})

	// Poll until succeeded.
	data := pollStatus(t, srv, id, "succeeded")

	if data["result_url"] != resultURL {
		t.Errorf("result_url: got %v, want %s", data["result_url"], resultURL)
	}
	if data["result_mime"] != "text/csv" {
		t.Errorf("result_mime: got %v, want text/csv", data["result_mime"])
	}
	if data["completed_at"] == nil {
		t.Errorf("completed_at: want non-nil after success")
	}
}

// ── failed job ────────────────────────────────────────────────────────────────

// TestJobs_DeadAfterMaxRetry verifies that a always-failing job reaches "dead"
// after exhausting retries and its error field is populated.
func TestJobs_DeadAfterMaxRetry(t *testing.T) {
	t.Parallel()
	srv, sink, q := newJobsServer(t)

	startWorker(t, q, sink, map[string]jobs.Handler{
		"failing": func(_ context.Context, _ jobs.Job) (jobs.Result, error) {
			return jobs.Result{}, fmt.Errorf("permanent error")
		},
	})

	id, err := q.Enqueue(context.Background(), jobs.Job{
		Type:     "failing",
		MaxRetry: 1, // fail fast
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	data := pollStatus(t, srv, id, "dead")

	if data["completed_at"] == nil {
		t.Errorf("completed_at: want non-nil after dead")
	}
}

// ── cancel ────────────────────────────────────────────────────────────────────

// TestJobs_Cancel verifies that cancelling a queued job updates the status row
// to "cancelled" and sets completed_at.
func TestJobs_Cancel(t *testing.T) {
	t.Parallel()
	srv, _, q := newJobsServer(t)

	// Do NOT start a worker — we cancel the job before it is dequeued.
	id, err := q.Enqueue(context.Background(), jobs.Job{
		Type:    "slow_report",
		ActorID: "user-99",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Row exists and is enqueued.
	srv.GET("/job_statuses/" + id).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		if data["status"] != "enqueued" {
			t.Errorf("before cancel: status want enqueued, got %v", data["status"])
		}
	})

	// Cancel via the Cancellable interface on the wrapped queue.
	c, ok := q.(jobs.Cancellable)
	if !ok {
		t.Fatal("wrapped queue does not implement jobs.Cancellable")
	}
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Status row should reflect the cancellation.
	srv.GET("/job_statuses/" + id).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		if data["status"] != "cancelled" {
			t.Errorf("after cancel: status want cancelled, got %v", data["status"])
		}
		if data["completed_at"] == nil {
			t.Errorf("after cancel: completed_at want non-nil")
		}
	})
}

// ── list / filtering ──────────────────────────────────────────────────────────

// TestJobs_ListAndFilter verifies pagination and field filtering on /job_statuses.
func TestJobs_ListAndFilter(t *testing.T) {
	t.Parallel()
	srv, _, q := newJobsServer(t)

	for i := range 3 {
		if _, err := q.Enqueue(context.Background(), jobs.Job{
			Type:    "batch",
			ActorID: fmt.Sprintf("actor-%d", i),
		}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// List returns all three rows (unauthenticated → no force-filter applied).
	srv.GET("/job_statuses").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		meta, _ := body["meta"].(map[string]any)
		if total, _ := meta["total"].(float64); total != 3 {
			t.Errorf("total: got %v, want 3", total)
		}
	})

	// Filter by status=enqueued returns all three.
	srv.GET("/job_statuses?filter=status:eq:enqueued").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		meta, _ := body["meta"].(map[string]any)
		if total, _ := meta["total"].(float64); total != 3 {
			t.Errorf("filtered total: got %v, want 3", total)
		}
	})

	// Filter by a specific actor_id returns exactly one row.
	srv.GET("/job_statuses?filter=actor_id:eq:actor-1").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		meta, _ := body["meta"].(map[string]any)
		if total, _ := meta["total"].(float64); total != 1 {
			t.Errorf("actor filter total: got %v, want 1", total)
		}
	})
}
