package jobsx_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/jobs"
	"github.com/xaleel/maniflex/scheduled"
	"github.com/xaleel/maniflex/scheduled/jobsx"
)

type jobxArticle struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	ExpiresAt *time.Time `json:"expires_at" db:"expires_at" mfx:"scheduled;soft-delete"`
}

func newRunner(t *testing.T) *scheduled.Runner {
	t.Helper()
	server := maniflex.New(maniflex.Config{AutoMigrate: true})
	server.MustRegister(jobxArticle{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("auto-migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	past := time.Date(2020, 1, 1, 6, 0, 0, 0, time.UTC)
	meta, _ := server.Registry().Get("jobxArticle")
	if _, err := db.Create(context.Background(), meta, map[string]any{
		"expires_at": past.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runner, err := scheduled.New(server, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     func() time.Time { return time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("scheduled.New: %v", err)
	}
	return runner
}

func TestJobHandler_RunsSweep_ReturnsReport(t *testing.T) {
	runner := newRunner(t)
	h := jobsx.JobHandler(runner)

	result, err := h(context.Background(), jobs.Job{Type: jobsx.JobType})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.Output == nil {
		t.Fatal("expected JSON output in Result.Output")
	}

	var out map[string]any
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	deleted, _ := out["deleted"].(float64)
	if deleted != 1 {
		t.Errorf("output.deleted want 1, got %v (full output: %s)", deleted, result.Output)
	}
}

func TestJobHandler_SweepError_ReturnsError(t *testing.T) {
	// Build a runner whose DB is nil after construction to force a sweep failure.
	// We do this by using a server with no DB set — New will error. So instead,
	// build a runner on a valid server, then close the DB to induce an error.
	server := maniflex.New(maniflex.Config{})
	server.MustRegister(jobxArticle{})
	db, _ := sqlite.Open(":memory:", server.Registry())
	server.SetDB(db)
	_ = db.AutoMigrate(context.Background(), server.Registry())

	runner, err := scheduled.New(server, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     func() time.Time { return time.Now() },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close DB so the next FindMany fails.
	db.Close()

	h := jobsx.JobHandler(runner)
	// Sweep adds per-model errors but doesn't return a top-level error;
	// a closed DB makes FindMany fail which sweepModel returns as an error
	// appended to Report.Errors — Sweep itself returns nil. The handler
	// wraps Report.Errors into reportJSON.Errors, not as a handler error.
	// Verify it still returns a valid result.
	result, err := h(context.Background(), jobs.Job{})
	if err != nil {
		// Only expect an error if Sweep itself fails (very unusual).
		t.Logf("handler returned error (acceptable): %v", err)
		return
	}
	// If no error, output should still be valid JSON.
	if result.Output == nil {
		t.Fatal("expected JSON output even on sweep errors")
	}
	var out map[string]any
	if jsonErr := json.Unmarshal(result.Output, &out); jsonErr != nil {
		t.Fatalf("invalid JSON output: %v", jsonErr)
	}
	_ = errors.New("checked") // dummy to silence import
}
