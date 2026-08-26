package e2e

// Coverage for the FilesConfig refactor: the new KeyGen hook, the explicit
// MountEndpoints gate (and its migration footgun), and the Before/After
// middleware split — including the streaming-safe response handling that stops
// an after-middleware from corrupting an already-sent body.

import (
	"encoding/json"
	"errors"
	"io"
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

// ── The response writer the file routes hand to middleware ───────────────────
//
// The file routes wrap the ResponseWriter so an AfterMiddleware can read the
// outcome without the body being buffered. A wrapper that only forwards
// Write/WriteHeader silently drops every optional interface the concrete
// net/http writer implements, and one of them is load-bearing here: Serve ends
// in io.Copy, which reaches the destination's io.ReaderFrom — that is
// net/http's optimised path to the connection, and losing it puts every
// download through a generic 32KB user-space loop instead (audit M6).
//
// Flusher and Hijacker come along for free and are asserted with it, so a
// future wrapper cannot quietly narrow the writer again.

func TestFilesConfig_ResponseWriterPreservesStreamingInterfaces(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	var seen, gotReaderFrom, gotFlusher, gotHijacker bool

	srv := testutil.NewServer(t, testutil.Options{
		Models: testutil.FileModels(),
		FilesConfig: &maniflex.FilesConfig{
			Storage:        store,
			MountEndpoints: true,
			BeforeMiddlewares: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					seen = true
					_, gotReaderFrom = ctx.Writer.(io.ReaderFrom)
					_, gotFlusher = ctx.Writer.(http.Flusher)
					_, gotHijacker = ctx.Writer.(http.Hijacker)
					return next()
				},
			},
		},
	})

	resp := srv.POSTMultipart("/files", nil, txtUpload("a.txt"))
	resp.AssertStatus(http.StatusCreated)
	key := testutil.Field(t, resp.Data(), "key")

	srv.GET("/files/" + key).AssertStatus(http.StatusOK)

	if !seen {
		t.Fatal("the before-middleware never ran, so nothing about ctx.Writer was observed")
	}
	if !gotReaderFrom {
		t.Error("ctx.Writer on a file route is not an io.ReaderFrom: writeFileResponse ends in " +
			"io.Copy, which uses the destination's ReadFrom when it has one, so every download " +
			"falls back to a generic user-space copy loop instead of net/http's own path")
	}
	if !gotFlusher {
		t.Error("ctx.Writer on a file route is not an http.Flusher")
	}
	if !gotHijacker {
		t.Error("ctx.Writer on a file route is not an http.Hijacker")
	}
}

// The trap in fixing the above: bytes written through ReadFrom bypass Write, so
// a wrapper that tracks "did anything go out?" only in Write reports false after
// streaming a whole file. Two guards in wrapFileMiddleware depend on that answer,
// and both then staple a 500 envelope onto a body already on the wire -- the M2
// corruption, reintroduced on the file path by the fix for M6.
//
// This targets the upload rather than the download deliberately. writeFileResponse
// sets Content-Length whenever the size is known, and net/http silently discards
// writes past it, so a download of a sized file masks the second write instead of
// showing it. The 201 upload response declares no length up front, so a stacked
// envelope lands in the body where it can be seen -- as does a download whose
// FileMeta carries no size, since that Content-Length is conditional.
func TestFilesConfig_FailureAfterStreamingDoesNotStackAnEnvelope(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()

	srv := testutil.NewServer(t, testutil.Options{
		Models: testutil.FileModels(),
		FilesConfig: &maniflex.FilesConfig{
			Storage:        store,
			MountEndpoints: true,
			AfterMiddlewares: []maniflex.MiddlewareFunc{
				func(ctx *maniflex.ServerContext, next func() error) error {
					// Fails only once the handler has committed its response.
					return errors.New("audit sink unreachable")
				},
			},
		},
	})

	resp := srv.POSTMultipart("/files", nil, txtUpload("a.txt"))
	body := string(resp.Body)

	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d, want 201: it was already sent before the middleware failed", resp.Status)
	}
	if strings.Contains(body, "INTERNAL") {
		t.Errorf("a 500 error envelope was stacked onto a response that was already committed; "+
			"the writer reported nothing written after the handler wrote its body.\nbody: %q", body)
	}
	var probe any
	if err := json.Unmarshal(resp.Body, &probe); err != nil {
		t.Errorf("the response body is not valid JSON (%v) -- two objects concatenated is exactly "+
			"what a second write onto a committed response produces.\nbody: %q", err, body)
	}

	// And the download still streams its bytes intact through the new wrapper.
	key := testutil.Field(t, resp.Data(), "key")
	if got := string(srv.GET("/files/" + key).Body); got != "hello" {
		t.Errorf("download body = %q, want the stored contents", got)
	}
}
