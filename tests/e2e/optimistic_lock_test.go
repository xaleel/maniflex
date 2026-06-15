package e2e

// TestOptimisticLock covers roadmap item 5.9 — If-Match / ETag optimistic
// concurrency control for PATCH and DELETE.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestOptimisticLock

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/middleware/response"
	"maniflex/tests/e2e/testutil"
)

// optDoc is the model used across all optimistic-lock tests.
type optDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required,filterable"`
	Notes string `json:"notes" db:"notes" mfx:"filterable"`
}

// newOptLockServer builds a test server with OptimisticLock enabled on optDoc
// and response.Cache wired so GET responses carry an ETag header.
func newOptLockServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			optDoc{},
			maniflex.ModelConfig{OptimisticLock: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				response.Cache(0), // max-age=0 — we only care about ETags, not caching
				maniflex.ForModel("optDoc"),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})
}

// ── Happy-path ────────────────────────────────────────────────────────────────

func TestOptimisticLock_PatchWithCorrectETagSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "original"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not return an ETag header")
	}

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "updated"},
		map[string]string{"If-Match": etag}).
		AssertStatus(http.StatusOK)
}

func TestOptimisticLock_DeleteWithCorrectETagSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "to-delete"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not return an ETag header")
	}

	srv.DELETE("/opt_docs/"+id, map[string]string{"If-Match": etag}).
		AssertStatus(http.StatusNoContent)
}

// ── 412 on stale ETag ─────────────────────────────────────────────────────────

func TestOptimisticLock_PatchWithWrongETagReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"},
		map[string]string{"If-Match": `"not-the-right-etag"`}).
		AssertStatus(http.StatusPreconditionFailed)
}

func TestOptimisticLock_DeleteWithWrongETagReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.DELETE("/opt_docs/"+id, map[string]string{"If-Match": `"wrong"`}).
		AssertStatus(http.StatusPreconditionFailed)
}

func TestOptimisticLock_412ResponseHasPreconditionFailedCode(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "doc"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	resp := srv.PATCH("/opt_docs/"+id, map[string]any{"notes": "x"},
		map[string]string{"If-Match": `"stale"`})
	resp.AssertStatus(http.StatusPreconditionFailed)
	resp.AssertJSON(func(body map[string]any) {
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected 'error' object, got %T", body["error"])
		}
		if errObj["code"] != "PRECONDITION_FAILED" {
			t.Errorf("error.code: got %q, want PRECONDITION_FAILED", errObj["code"])
		}
	})
}

// ── No If-Match header → no enforcement ──────────────────────────────────────

func TestOptimisticLock_PatchWithoutIfMatchSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"}).
		AssertStatus(http.StatusOK)
}

func TestOptimisticLock_DeleteWithoutIfMatchSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.DELETE("/opt_docs/" + id).AssertStatus(http.StatusNoContent)
}

// ── ETag freshness ────────────────────────────────────────────────────────────

func TestOptimisticLock_ETagChangesAfterUpdate(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag1 := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"},
		map[string]string{"If-Match": etag1}).
		AssertStatus(http.StatusOK)
	etag2 := srv.GET("/opt_docs/" + id).Header.Get("ETag")

	if etag1 == etag2 {
		t.Error("ETag must change after an update")
	}
}

func TestOptimisticLock_StaleETagAfterUpdateReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	staleETag := srv.GET("/opt_docs/" + id).Header.Get("ETag")

	// A concurrent writer updates the record.
	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"}).
		AssertStatus(http.StatusOK)

	// Original ETag is now stale — must get 412.
	srv.PATCH("/opt_docs/"+id, map[string]any{"notes": "late update"},
		map[string]string{"If-Match": staleETag}).
		AssertStatus(http.StatusPreconditionFailed)
}

// ── Opt-out: model without OptimisticLock ignores If-Match ────────────────────

func TestOptimisticLock_ModelWithoutFlagIgnoresIfMatch(t *testing.T) {
	t.Parallel()
	// Default test server uses the standard fixtures which do NOT set OptimisticLock.
	srv := testutil.NewServer(t, testutil.Options{})

	id := srv.CreateUser("Alice", "alice-optlock@test.com", "viewer").
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	// Supply a deliberately wrong ETag — should be ignored because OptimisticLock is not set.
	srv.PATCH("/users/"+id, map[string]any{"name": "Bob"},
		map[string]string{"If-Match": `"irrelevant-etag"`}).
		AssertStatus(http.StatusOK)
}
