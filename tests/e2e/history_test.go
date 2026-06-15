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

func histServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{HistNote{}, HistDiffOnly{}},
	})
}

// TestHistory_CreateWritesHistoryRow verifies a history row is created on POST.
func TestHistory_CreateWritesHistoryRow(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Hello", "body": "World",
	}))

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s", id)).DataList()
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

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s&sort=version:asc", id)).DataList()
	testutil.AssertLen(t, "history rows after update", items, 2)

	update := items[1].(map[string]any)
	testutil.AssertEqual(t, "operation", update["operation"], "update")
	testutil.AssertEqual(t, "version",   update["version"], float64(2))
}

// TestHistory_DeleteWritesHistoryRow verifies a history row on DELETE.
func TestHistory_DeleteWritesHistoryRow(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Bye", "body": "gone",
	}))
	srv.DELETE("/hist_notes/" + id).AssertStatus(http.StatusNoContent)

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s&sort=version:asc", id)).DataList()
	testutil.AssertLen(t, "history rows after delete", items, 2)

	del := items[1].(map[string]any)
	testutil.AssertEqual(t, "operation", del["operation"], "delete")
	testutil.AssertEqual(t, "version",   del["version"], float64(2))
}

// TestHistory_SnapshotPresentWhenNotDiffOnly checks snapshot column is populated.
func TestHistory_SnapshotPresentWhenNotDiffOnly(t *testing.T) {
	t.Parallel()
	srv := histServer(t)

	id := srv.MustID(srv.POST("/hist_notes", map[string]any{
		"title": "Snap", "body": "shot",
	}))

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s", id)).DataList()
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

	items := srv.GET(fmt.Sprintf("/hist_diff_only_history?filter=record_id:eq:%s", id)).DataList()
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

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s&sort=version:asc", id)).DataList()
	testutil.AssertLen(t, "history rows", items, 3)
	for i, item := range items {
		row := item.(map[string]any)
		testutil.AssertEqual(t, fmt.Sprintf("version[%d]", i), row["version"], float64(i+1))
	}
}

// TestHistory_HistoryTableReadOnly verifies that POST to the history table returns 405.
func TestHistory_HistoryTableReadOnly(t *testing.T) {
	t.Parallel()
	srv := histServer(t)
	srv.POST("/hist_note_history", map[string]any{}).AssertStatus(http.StatusMethodNotAllowed)
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

	items := srv.GET(fmt.Sprintf("/hist_note_history?filter=record_id:eq:%s&sort=version:asc&limit=100", id)).DataList()
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
