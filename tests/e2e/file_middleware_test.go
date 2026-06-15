package e2e

// Regression coverage for roadmap §11A.6 (checkpoint C6): Config.FileMiddleware
// wraps the standalone /files endpoints so callers can apply auth (or any
// other ServerContext-based middleware) to file upload/download/delete.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestFileMiddleware_WrapsStandaloneRoutes(t *testing.T) {
	t.Parallel()

	t.Run("middleware_short_circuit_blocks_upload", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
			FileMiddleware: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "auth required")
					return nil
				},
			},
		})

		resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "blocked.txt", ContentType: "text/plain", Body: []byte("nope")},
		})
		resp.AssertStatus(http.StatusUnauthorized)

		// Nothing should have been written to storage — the file handler
		// never ran because the middleware short-circuited.
		if len(store.Keys()) != 0 {
			t.Errorf("expected no files in storage after blocked upload, got %d", len(store.Keys()))
		}
	})

	t.Run("middleware_short_circuit_blocks_delete", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()

		// Seed a file via a server WITHOUT middleware so the upload succeeds.
		seed := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
		})
		upload := seed.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "keep.txt", ContentType: "text/plain", Body: []byte("survive")},
		})
		upload.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, upload.Data(), "key")

		// New server with the same storage and a deny-all middleware.
		guarded := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
			FileMiddleware: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusForbidden, "FORBIDDEN", "delete not permitted")
					return nil
				},
			},
		})

		guarded.DELETE("/files/" + key).AssertStatus(http.StatusForbidden)

		if !store.HasKey(key) {
			t.Errorf("file %q should still exist after blocked delete", key)
		}
	})

	t.Run("middleware_pass_through_allows_upload", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		var ran bool
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
			FileMiddleware: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					ran = true
					return next()
				},
			},
		})

		resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "ok.txt", ContentType: "text/plain", Body: []byte("yes")},
		})
		resp.AssertStatus(http.StatusCreated)

		if !ran {
			t.Error("FileMiddleware was not invoked")
		}
		if len(store.Keys()) != 1 {
			t.Errorf("expected 1 file in storage, got %d", len(store.Keys()))
		}
	})

	t.Run("default_no_middleware_preserves_existing_behaviour", func(t *testing.T) {
		// Backward-compat: empty FileMiddleware leaves /files unauthenticated,
		// matching pre-fix behaviour so existing deployments keep working.
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
		})

		resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "anyone.txt", ContentType: "text/plain", Body: []byte("yes")},
		})
		resp.AssertStatus(http.StatusCreated)
	})
}
