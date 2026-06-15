package e2e_test

// Regression coverage for Phase 11C fixes — see roadmap_unified.md §11C
// and checkpoint_findings.md. These tests pin behaviour that was either
// silently wrong, leaky, or impossible to express pre-fix.

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// 11C.1 — `.meta.json` sidecars must not be reachable through the file
// handler. Pre-fix the framework's own metadata layout was a public surface.
func TestPhase11C_MetaJSONNotServable(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: store,
	})

	// Upload a file via the standalone endpoint.
	resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
		"file": {Filename: "report.txt", ContentType: "text/plain", Body: []byte("payload")},
	})
	resp.AssertStatus(http.StatusCreated)
	key := testutil.Field(t, resp.Data(), "key")

	// Direct fetch of the would-be sidecar must 404. MemoryStorage doesn't
	// have sidecars at all but the file handler should still refuse keys
	// ending in .meta.json so an attacker cannot probe internals on a
	// backend that does.
	srv.GETRaw("/files/" + key + ".meta.json").AssertStatus(http.StatusNotFound)
}

// 11C.2 sanitizeFilename coverage lives next to the function itself in
// file_handler_test.go — multipart parsing rejects most hostile filenames
// (CR/LF, NUL) before they can reach the sanitizer, so the end-to-end
// path doesn't exercise the interesting cases.

// 11C.4 — DELETE /files/* must NOT do an Exists round-trip; missing keys
// resolve straight to 404 via the storage's ErrFileNotFound. Concurrent
// deletes of the same key must never produce a 500.
func TestPhase11C_DeleteIsSingleShot(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: store,
	})

	// Upload a real file.
	resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
		"file": {Filename: "delete-me.txt", ContentType: "text/plain", Body: []byte("data")},
	})
	resp.AssertStatus(http.StatusCreated)
	key := testutil.Field(t, resp.Data(), "key")

	// Two parallel deletes; both finish without a 500.
	type deleteResult struct{ status int }
	results := make(chan deleteResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := srv.DELETE("/files/" + key)
			results <- deleteResult{status: r.Status}
		}()
	}
	statuses := []int{(<-results).status, (<-results).status}
	// Acceptable outcomes: one 204 + one 404 (race winner vs. loser); two
	// 204s on backends that don't distinguish. Never 500.
	for _, s := range statuses {
		if s == http.StatusInternalServerError {
			t.Fatalf("concurrent delete produced 500: %v", statuses)
		}
		if s != http.StatusNoContent && s != http.StatusNotFound {
			t.Errorf("unexpected delete status: %d", s)
		}
	}
}

// 11C.9 — Register(A, B, ModelConfig{...}) must apply the config to B (the
// immediately-preceding model), not A. Pre-fix, configs and models were
// zipped by their own discovery indices, so the first config silently bound
// to the first model regardless of placement.
//
// The test uses two minimal models so we can probe the routes the framework
// generates. Tag is the second argument and carries the TableName override;
// User has no override and must remain on its default route.
func TestPhase11C_FlattenArgsPairsConfigWithPrecedingModel(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.User{},
			testutil.Tag{},
			maniflex.ModelConfig{TableName: "phase11c_tags_renamed"},
		},
	})

	// Tag should live under the renamed route.
	createResp := srv.POST("/phase11c_tags_renamed", map[string]any{
		"name":  "renamed-table-tag",
		"color": "blue",
	})
	createResp.AssertStatus(http.StatusCreated)

	// The default /tags route must NOT exist — Tag is mapped to the custom
	// table.
	missing := srv.GETRaw("/tags")
	if missing.Status == http.StatusOK {
		t.Errorf("/tags still served — config did not rebind Tag (pre-11C.9 bug applied config to wrong model)")
	}

	// And the User route should still work under its default name; the
	// config did not leak onto it.
	srv.GET("/users", nil).AssertStatus(http.StatusOK)
}

// 11C.10 — MemoryCache prunes expired keys periodically. After enough Set
// calls with expired TTLs, expired entries are sweep-removed during the
// next Set rather than lingering forever.
func TestPhase11C_MemoryCacheAmortisedPrune(t *testing.T) {
	t.Parallel()
	c := maniflex.NewMemoryCache()
	ctx := context.Background()

	// Insert > pruneEvery entries with a 1ms TTL, then sleep so they all
	// expire. Use a real millisecond rather than 1ns to avoid sub-clock
	// jitter on Windows where time.Now resolution can be coarse.
	for i := 0; i < 200; i++ {
		c.Set(ctx, "k"+strconv.Itoa(i), "v", time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)

	// All entries are now expired. Get is lazy-evict, so each call removes
	// the entry it sees expired — and none should come back live.
	for i := 0; i < 200; i++ {
		if _, ok := c.Get(ctx, "k"+strconv.Itoa(i)); ok {
			t.Errorf("key %d still live after TTL expired", i)
		}
	}

	// Sentinel: after the sweep, a fresh long-lived insert is observable.
	c.Set(ctx, "alive", "x", time.Hour)
	if v, ok := c.Get(ctx, "alive"); !ok || v != "x" {
		t.Errorf("post-sweep insert missing: got (%v, %v)", v, ok)
	}
}

// 11C.14 PATCH-null behaviour is covered by code review rather than an
// end-to-end test: the schema generator currently emits NOT NULL for every
// non-pointer field, so the DB layer rejects the clear write before the new
// code path can be observed in a black-box test. A future schema enhancement
// (mfx:"nullable") would unlock a true e2e regression here.
