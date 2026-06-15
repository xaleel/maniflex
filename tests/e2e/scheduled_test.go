package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/scheduled"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Models ────────────────────────────────────────────────────────────────────

type e2eArticle struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Status    string     `json:"status"     db:"status"     mfx:"enum:draft|published,filterable"`
	PublishAt *time.Time `json:"publish_at" db:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`
	ExpiresAt *time.Time `json:"expires_at" db:"expires_at" mfx:"scheduled;soft-delete"`
}

type e2eBanner struct {
	maniflex.BaseModel
	Color        string     `json:"color"         db:"color"         mfx:"required,filterable"`
	HolidayStart *time.Time `json:"holiday_start" db:"holiday_start" mfx:"scheduled;field=color;to=red"`
	HolidayEnd   *time.Time `json:"holiday_end"   db:"holiday_end"   mfx:"scheduled;field=color;from=red;to=blue"`
}

// ── Setup helper ──────────────────────────────────────────────────────────────

type e2eSchedSetup struct {
	srv    *testutil.Server
	db     maniflex.DBAdapter
	runner *scheduled.Runner
}

func newSchedServer(t *testing.T, clock func() time.Time) *e2eSchedSetup {
	t.Helper()

	var capturedDB maniflex.DBAdapter

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{e2eArticle{}, e2eBanner{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			db, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			capturedDB = db
			return db, nil
		},
	})

	// Build the runner using the same server Server instance.
	// srv is an httptest.Server wrapper — we need the underlying maniflex.Server.
	// Access it indirectly: build a fresh runner by re-using the testutil
	// server's exported Maniflex accessor if available, else use a parallel Server.
	//
	// Since testutil.Server doesn't expose the Server, we create a standalone runner
	// pointed at the same DB and registry by creating a minimal Maniflex wrapper.
	mfxSrv := maniflex.New(maniflex.Config{AutoMigrate: false})
	mfxSrv.MustRegister(e2eArticle{}, e2eBanner{})
	mfxSrv.SetDB(capturedDB)

	runner, err := scheduled.New(mfxSrv, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     clock,
	})
	if err != nil {
		t.Fatalf("scheduled.New: %v", err)
	}

	return &e2eSchedSetup{srv: srv, db: capturedDB, runner: runner}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// ── Auto-index existence ──────────────────────────────────────────────────────

func TestScheduled_E2E_AutoMigrateCreatesIndexes(t *testing.T) {
	t.Parallel()

	// Verify that AutoMigrate succeeds for models with scheduled tags (which
	// auto-inject IndexSpec entries). A healthy /health response confirms it.
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{e2eArticle{}, e2eBanner{}},
	})
	srv.GET("/health").AssertStatus(http.StatusOK)
}

// ── Publish-at: set-field with from ──────────────────────────────────────────

func TestScheduled_E2E_PublishAt_FlipsStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	past := time.Date(2025, 6, 1, 6, 0, 0, 0, time.UTC)
	setup := newSchedServer(t, fixedClock(now))

	// Create article with status=draft and a past publish_at.
	resp := setup.srv.POST("/e2e_articles", map[string]any{
		"status":     "draft",
		"publish_at": past.Format(time.RFC3339),
	})
	resp.AssertStatus(http.StatusCreated)
	id := resp.ID()

	// Run one sweep.
	rep, err := setup.runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rep.Updated != 1 {
		t.Errorf("Report.Updated want 1, got %d (errors: %v)", rep.Updated, rep.Errors)
	}

	// The API should now return status=published.
	setup.srv.GET(fmt.Sprintf("/e2e_articles/%s", id)).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		if data["status"] != "published" {
			t.Errorf("status want published, got %v", data["status"])
		}
	})
}

// ── ExpiresAt: soft-delete ────────────────────────────────────────────────────

func TestScheduled_E2E_ExpiresAt_SoftDeletes(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	past := time.Date(2025, 6, 1, 6, 0, 0, 0, time.UTC)
	setup := newSchedServer(t, fixedClock(now))

	resp := setup.srv.POST("/e2e_articles", map[string]any{
		"status":     "draft",
		"expires_at": past.Format(time.RFC3339),
	})
	resp.AssertStatus(http.StatusCreated)
	id := resp.ID()

	rep, err := setup.runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rep.Deleted != 1 {
		t.Errorf("Report.Deleted want 1, got %d (errors: %v)", rep.Deleted, rep.Errors)
	}

	// Article is soft-deleted — GET returns 404, list omits it.
	setup.srv.GET(fmt.Sprintf("/e2e_articles/%s", id)).AssertStatus(http.StatusNotFound)
	setup.srv.GET("/e2e_articles").AssertJSON(func(body map[string]any) {
		meta, _ := body["meta"].(map[string]any)
		total, _ := meta["total"].(float64)
		if total != 0 {
			t.Errorf("list total want 0 after soft-delete, got %v", total)
		}
	})
}

// ── Banner: chained set-field transitions ─────────────────────────────────────

func TestScheduled_E2E_BannerChained(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	past := time.Date(2025, 6, 1, 6, 0, 0, 0, time.UTC)
	setup := newSchedServer(t, fixedClock(now))

	resp := setup.srv.POST("/e2e_banners", map[string]any{
		"color":         "green",
		"holiday_start": past.Format(time.RFC3339),
		"holiday_end":   past.Format(time.RFC3339),
	})
	resp.AssertStatus(http.StatusCreated)
	id := resp.ID()

	// Two sweeps needed: first sets red, second sets blue.
	setup.runner.Sweep(context.Background()) //nolint:errcheck
	setup.runner.Sweep(context.Background()) //nolint:errcheck

	setup.srv.GET(fmt.Sprintf("/e2e_banners/%s", id)).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		if data["color"] != "blue" {
			t.Errorf("color want blue after two sweeps, got %v", data["color"])
		}
	})
}
