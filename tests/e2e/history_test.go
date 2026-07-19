package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Test models ───────────────────────────────────────────────────────────────

type HistNote struct {
	maniflex.BaseModel `mfx:"versioned"`
	Title         string `json:"title" db:"title" mfx:"required,filterable"`
	Body          string `json:"body"  db:"body"`
}

type HistDiffOnly struct {
	maniflex.BaseModel `mfx:"versioned:diff_only"`
	Label         string `json:"label" db:"label" mfx:"required"`
}

// HistSoftNote soft-deletes, so its row survives a DELETE and can still
// authorise a read of its own history (audit MS-4).
type HistSoftNote struct {
	maniflex.BaseModel `mfx:"versioned"`
	maniflex.WithDeletedAt
	Title string `json:"title" db:"title"`
}

func histServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{HistNote{}, HistDiffOnly{}, HistSoftNote{}},
	})
}

// histRowsAsc reads a record's history through GET /:model/{id}/history and
// returns it oldest-first.
//
// The endpoint answers newest-first — the question a history view is usually
// asked — while these tests were written against the old flat endpoint's
// explicit &sort=version:asc, so they reverse. The flat /{model}_history route
// is gone: it was unauthenticated and unscoped (audit MS-4), and the history
// table has no tenant column to scope it by, so it is reached through the
// parent record instead.
func histRowsAsc(t *testing.T, srv *testutil.Server, path, id string) []any {
	t.Helper()
	items := srv.GET(fmt.Sprintf("%s/%s/history?limit=100", path, id)).DataList()
	out := make([]any, len(items))
	for i, it := range items {
		out[len(items)-1-i] = it
	}
	return out
}

// TestHistory_CreateWritesHistoryRow verifies a history row is created on POST.
func TestHistory_CreateWritesHistoryRow(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Hello", "body": "World",
	}))

	items := histRowsAsc(t, srv, "/hist_notes", id)
	testutil.AssertLen(t, "history rows after create", items, 1)

	row := items[0].(map[string]any)
	testutil.AssertEqual(t, "operation", row["operation"], "create")
	testutil.AssertEqual(t, "version",   row["version"], float64(1))
	testutil.AssertEqual(t, "record_id", row["record_id"], id)

	diff, ok := row["diff"].(string)
	if !ok || diff == "" {
		t.Errorf("expected non-empty diff string, got %v", row["diff"])
	}
}

// TestHistory_UpdateWritesHistoryRow verifies a second history row on PATCH.
func TestHistory_UpdateWritesHistoryRow(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Original", "body": "v1",
	}))
	srv.PATCH("/hist_notes/"+id, map[string]any{"title": "Updated"}).AssertStatus(http.StatusOK)

	items := histRowsAsc(t, srv, "/hist_notes", id)
	testutil.AssertLen(t, "history rows after update", items, 2)

	update := items[1].(map[string]any)
	testutil.AssertEqual(t, "operation", update["operation"], "update")
	testutil.AssertEqual(t, "version",   update["version"], float64(2))
}

// TestHistory_DeleteWritesHistoryRow verifies a history row on DELETE.
//
// It uses the soft-delete model because the history endpoint authorises through
// the parent record (audit MS-4): a soft-deleted row is still there to authorise
// against — the adapter's ScopeChecker counts it as present while still applying
// tenancy — so its history, including the delete entry, stays readable by exactly
// the callers who could read the record.
func TestHistory_DeleteWritesHistoryRow(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_soft_notes", map[string]any{"title": "Bye"}))
	srv.DELETE("/hist_soft_notes/" + id).AssertStatus(http.StatusNoContent)

	// The record itself is gone from the read path...
	srv.GET("/hist_soft_notes/" + id).AssertStatus(http.StatusNotFound)

	// ...but its history is not, which is the point of keeping the row.
	items := histRowsAsc(t, srv, "/hist_soft_notes", id)
	testutil.AssertLen(t, "history rows after delete", items, 2)

	del := items[1].(map[string]any)
	testutil.AssertEqual(t, "operation", del["operation"], "delete")
	testutil.AssertEqual(t, "version",   del["version"], float64(2))
}

// TestHistory_HardDeletedRecordHistoryIsUnreachable pins the documented boundary
// of the MS-4 gate rather than leaving it to be discovered.
//
// A hard delete removes the only row that could say who is allowed to read this
// record, so there is nothing left to authorise against. Answering from the
// history table alone would mean choosing between showing it to everyone and
// showing it to no one; the endpoint chooses no one, and the rows stay in the
// table for an admin query or an offline audit.
func TestHistory_HardDeletedRecordHistoryIsUnreachable(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Bye", "body": "gone",
	}))
	// History is readable while the record exists.
	testutil.AssertLen(t, "history before delete",
		histRowsAsc(t, srv, "/hist_notes", id), 1)

	srv.DELETE("/hist_notes/" + id).AssertStatus(http.StatusNoContent)

	srv.GET("/hist_notes/"+id+"/history").AssertStatus(http.StatusNotFound)
}

// TestHistory_SnapshotPresentWhenNotDiffOnly checks snapshot column is populated.
func TestHistory_SnapshotPresentWhenNotDiffOnly(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Snap", "body": "shot",
	}))

	items := histRowsAsc(t, srv, "/hist_notes", id)
	testutil.AssertLen(t, "history rows", items, 1)

	row := items[0].(map[string]any)
	if row["snapshot"] == nil || row["snapshot"] == "" {
		t.Errorf("expected snapshot to be populated, got %v", row["snapshot"])
	}
}

// TestHistory_DiffOnlyHasNoSnapshot verifies snapshot is absent for diff_only models.
func TestHistory_DiffOnlyHasNoSnapshot(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_diff_onlies", map[string]any{"label": "test"}))

	items := histRowsAsc(t, srv, "/hist_diff_onlies", id)
	testutil.AssertLen(t, "history rows", items, 1)

	row := items[0].(map[string]any)
	if _, has := row["snapshot"]; has {
		t.Errorf("expected no snapshot field for diff_only model, got %v", row["snapshot"])
	}
}

// TestHistory_VersionIncrements verifies version numbers are monotonically increasing.
func TestHistory_VersionIncrements(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{"title": "v1", "body": ""}))
	srv.PATCH("/hist_notes/"+id, map[string]any{"title": "v2"}).AssertStatus(http.StatusOK)
	srv.PATCH("/hist_notes/"+id, map[string]any{"title": "v3"}).AssertStatus(http.StatusOK)

	items := histRowsAsc(t, srv, "/hist_notes", id)
	testutil.AssertLen(t, "history rows", items, 3)
	for i, item := range items {
		row := item.(map[string]any)
		testutil.AssertEqual(t, fmt.Sprintf("version[%d]", i), row["version"], float64(i+1))
	}
}

// TestHistory_FlatRouteIsGone replaces a test that asserted POST
// /hist_note_history returned 405 — the write-blocking middleware doing its job
// on a route that also served unauthenticated, unscoped reads (audit MS-4).
//
// The history model is Headless now, so the whole flat surface is absent and
// there is no write to block over HTTP. That is a stronger guarantee than the
// 405 was: a route that does not exist cannot be misconfigured into existing.
// The write blocker is kept for ctx.Execute, which reaches registered models
// without going through the router.
func TestHistory_FlatRouteIsGone(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	for _, probe := range []struct{ method, path string }{
		{"GET", "/hist_note_history"},
		{"POST", "/hist_note_history"},
		{"GET", "/hist_diff_only_history"},
	} {
		var resp *testutil.Response
		if probe.method == "GET" {
			resp = srv.GET(probe.path)
		} else {
			resp = srv.POST(probe.path, map[string]any{})
		}
		if resp.Status != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404 — the flat history route must not be mounted",
				probe.method, probe.path, resp.Status)
		}
	}
}

// TestHistory_ConcurrentUpdatesProduceUniqueVersions pins roadmap §11A.9:
// concurrent updates to the same record used to race on the non-transactional
// nextVersion computation and silently persist duplicate (record_id, version)
// rows. After the fix the history index is UNIQUE, so a collision surfaces as
// an *ErrConstraint that appendHistoryRow retries until the version is fresh.
// The post-condition is N history rows with distinct, contiguous versions
// 1..N+1 (1 for the create, +N for the updates).
func TestHistory_ConcurrentUpdatesProduceUniqueVersions(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{"title": "v1", "body": ""}))

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv.PATCH("/hist_notes/"+id, map[string]any{"title": fmt.Sprintf("v%d", i+2)}).
				AssertStatus(http.StatusOK)
		}(i)
	}
	wg.Wait()

	items := histRowsAsc(t, srv, "/hist_notes", id)
	testutil.AssertLen(t, "history rows", items, n+1)

	seen := make(map[int]bool, n+1)
	for _, item := range items {
		row := item.(map[string]any)
		v := int(row["version"].(float64))
		if seen[v] {
			t.Errorf("duplicate version %d in history", v)
		}
		seen[v] = true
	}
	for i := 1; i <= n+1; i++ {
		if !seen[i] {
			t.Errorf("missing version %d", i)
		}
	}
}
