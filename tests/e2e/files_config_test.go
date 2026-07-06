package e2e

// Coverage for the FilesConfig refactor: the new KeyGen hook, the explicit
// MountEndpoints gate (and its migration footgun), and the Before/After
// middleware split — including the streaming-safe response handling that stops
// an after-middleware from corrupting an already-sent body.

import (
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func txtUpload(name string) map[string]testutil.FileUpload {
	return map[string]testutil.FileUpload{
		"file": {Filename: name, ContentType: "text/plain", Body: []byte("hello")},
	}
}

// ── KeyGen ──────────────────────────────────────────────────────────────────

func TestFilesConfig_KeyGen(t *testing.T) {
	t.Parallel()

	t.Run("custom_keygen_determines_stored_key", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				KeyGen: func(_ *maniflex.ServerContext, h *multipart.FileHeader) string {
					return "custom/prefix/" + h.Filename
				},
			},
		})

		resp := srv.POSTMultipart("/files", nil, txtUpload("report.txt"))
		resp.AssertStatus(http.StatusCreated)

		key := testutil.Field(t, resp.Data(), "key")
		if key != "custom/prefix/report.txt" {
			t.Errorf("KeyGen not honoured: got key %q, want %q", key, "custom/prefix/report.txt")
		}
		if !store.HasKey("custom/prefix/report.txt") {
			t.Errorf("file not stored under KeyGen key; stored keys: %v", store.Keys())
		}
	})

	t.Run("keygen_receives_populated_request_context", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		var gotCtx *maniflex.ServerContext
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				KeyGen: func(ctx *maniflex.ServerContext, h *multipart.FileHeader) string {
					gotCtx = ctx
					return "k/" + h.Filename
				},
			},
		})

		srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusCreated)

		if gotCtx == nil {
			t.Fatal("KeyGen was never called (ctx not captured)")
		}
		if gotCtx.Request == nil {
			t.Error("KeyGen received a ServerContext with no Request")
		}
	})

	t.Run("nil_keygen_falls_back_to_default_layout", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
			},
		})

		resp := srv.POSTMultipart("/files", nil, txtUpload("a.txt"))
		resp.AssertStatus(http.StatusCreated)

		key := testutil.Field(t, resp.Data(), "key")
		if !strings.HasPrefix(key, "uploads/") || !strings.HasSuffix(key, "/a.txt") {
			t.Errorf("default key layout wrong: got %q, want uploads/<uuid>/a.txt", key)
		}
	})
}

func TestDefaultKeyGen_SanitisesFilename(t *testing.T) {
	t.Parallel()
	h := &multipart.FileHeader{Filename: "../etc/pa ss\nwd.txt"}
	key := maniflex.DefaultKeyGen(nil, h)

	if !strings.HasPrefix(key, "uploads/") {
		t.Errorf("key %q missing uploads/ prefix", key)
	}
	// The sanitised filename component must not carry traversal or control bytes.
	name := key[strings.LastIndex(key, "/")+1:]
	for _, bad := range []string{"..", "\n", " "} {
		if strings.Contains(name, bad) {
			t.Errorf("sanitised filename %q still contains %q", name, bad)
		}
	}
}

// ── MountEndpoints ───────────────────────────────────────────────────────────

func TestFilesConfig_MountEndpoints(t *testing.T) {
	t.Parallel()

	t.Run("storage_only_does_not_mount_standalone_files", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: false, // the migration footgun: storage set, endpoints off
			},
		})

		// Standalone route is absent → router 404 (not 501).
		srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusNotFound)

		// Model file features remain gated on Storage alone and still work.
		srv.POSTMultipart("/documents", map[string]string{"title": "t"}, map[string]testutil.FileUpload{
			"file": {Filename: "doc.txt", ContentType: "text/plain", Body: []byte("body")},
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("mounted_without_storage_returns_501", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				MountEndpoints: true, // routes exist, but no backend
			},
		})

		resp := srv.POSTMultipart("/files", nil, txtUpload("a.txt"))
		resp.AssertStatus(http.StatusNotImplemented)
		if code := resp.ErrorCode(); code != "NO_STORAGE" {
			t.Errorf("error code: got %q, want NO_STORAGE", code)
		}
	})

	t.Run("openapi_lists_files_paths_only_when_mounted", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()

		mounted := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{Storage: store, MountEndpoints: true},
		})
		unmounted := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{Storage: store, MountEndpoints: false},
		})

		if !specHasPath(t, mounted, "/files") {
			t.Error("mounted server: /files missing from OpenAPI spec")
		}
		if specHasPath(t, unmounted, "/files") {
			t.Error("unmounted server: /files should be absent from OpenAPI spec")
		}
	})
}

func specHasPath(t *testing.T, srv *testutil.Server, path string) bool {
	t.Helper()
	body := srv.GET("/openapi.json").Body
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse openapi spec: %v", err)
	}
	_, ok := spec.Paths[path]
	return ok
}

// ── Before / After middleware ────────────────────────────────────────────────

func TestFilesConfig_AfterMiddlewares(t *testing.T) {
	t.Parallel()

	t.Run("runs_after_handler_and_observes_status", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		var afterRan bool
		var seenStatus int
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				AfterMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						afterRan = true
						if s, ok := ctx.Writer.(interface{ Status() int }); ok {
							seenStatus = s.Status()
						}
						return next()
					},
				},
			},
		})

		srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusCreated)

		if !afterRan {
			t.Error("AfterMiddleware did not run")
		}
		if seenStatus != http.StatusCreated {
			t.Errorf("AfterMiddleware observed status %d, want %d", seenStatus, http.StatusCreated)
		}
	})

	t.Run("before_and_after_order_and_short_circuit", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		var order []string
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				BeforeMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						order = append(order, "before")
						return next()
					},
				},
				AfterMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						order = append(order, "after")
						return next()
					},
				},
			},
		})

		srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusCreated)

		if len(order) != 2 || order[0] != "before" || order[1] != "after" {
			t.Errorf("middleware order = %v, want [before after]", order)
		}
		if len(store.Keys()) != 1 {
			t.Errorf("expected handler to store 1 file, got %d", len(store.Keys()))
		}
	})

	t.Run("before_short_circuit_skips_handler_and_after", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		var afterRan bool
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				BeforeMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "nope")
						return nil
					},
				},
				AfterMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						afterRan = true
						return next()
					},
				},
			},
		})

		srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusUnauthorized)

		if afterRan {
			t.Error("AfterMiddleware ran despite before-middleware short-circuit")
		}
		if len(store.Keys()) != 0 {
			t.Errorf("nothing should have been stored, got %d", len(store.Keys()))
		}
	})

	t.Run("after_setting_response_cannot_corrupt_sent_body", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			FilesConfig: &maniflex.FilesConfig{
				Storage:        store,
				MountEndpoints: true,
				AfterMiddlewares: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						// Too late: the upload response is already on the wire.
						ctx.Abort(http.StatusInternalServerError, "LATE", "should be ignored")
						return nil
					},
				},
			},
		})

		resp := srv.POSTMultipart("/files", nil, txtUpload("a.txt"))
		// The handler's 201 must win; the late Abort is ignored, not stacked on.
		resp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, resp.Data(), "key")
		if key == "" {
			t.Errorf("response body corrupted by late after-middleware; body: %s", resp.Body)
		}
	})
}

// ── Migration: legacy behaviour via the new struct ───────────────────────────

func TestFilesConfig_ReproducesLegacyMiddlewareBehaviour(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models: testutil.FileModels(),
		FilesConfig: &maniflex.FilesConfig{
			Storage:        store,
			MountEndpoints: true,
			BeforeMiddlewares: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusForbidden, "FORBIDDEN", "denied")
					return nil
				},
			},
		},
	})

	srv.POSTMultipart("/files", nil, txtUpload("a.txt")).AssertStatus(http.StatusForbidden)
	if len(store.Keys()) != 0 {
		t.Errorf("blocked upload should store nothing, got %d", len(store.Keys()))
	}
}
