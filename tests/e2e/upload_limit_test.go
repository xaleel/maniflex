package e2e

// Multipart request-size limiting. ParseMultipartForm's argument bounds only the
// in-memory buffer — the overflow spools to temp files — so without a ceiling on
// the body itself an upload is effectively unbounded and fills the disk. Every
// multipart entry point must stop the read at the limit and answer 413.

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/body"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// uploadOf returns a text/plain part of exactly n bytes — under Document's
// per-field max_size:1MB, so only the request-level ceiling can reject it.
func uploadOf(n int) map[string]testutil.FileUpload {
	return map[string]testutil.FileUpload{
		"file": {
			Filename:    "big.txt",
			ContentType: "text/plain",
			Body:        bytes.Repeat([]byte("a"), n),
		},
	}
}

func uploadLimitServer(t *testing.T, store maniflex.FileStorage, limit int64,
	mw ...maniflex.MiddlewareFunc) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: testutil.FileModels(),
		FilesConfig: &maniflex.FilesConfig{
			Storage:        store,
			MountEndpoints: true,
			MaxUploadBytes: limit,
		},
		Middleware: func(s *maniflex.Server) {
			for _, m := range mw {
				s.Pipeline.Deserialize.Register(m)
			}
		},
	})
}

func TestUploadLimit_OversizedMultipartRejected(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := uploadLimitServer(t, store, 64<<10) // 64 KB request ceiling

	resp := srv.POSTMultipart("/documents", map[string]string{"title": "big"}, uploadOf(256<<10))
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "BODY_TOO_LARGE" {
		t.Errorf("error code: got %q, want BODY_TOO_LARGE", code)
	}

	// The read stopped at the ceiling, so nothing reached storage.
	if keys := store.Keys(); len(keys) != 0 {
		t.Errorf("stored %d objects, want 0 — the oversized upload was written", len(keys))
	}
	if items := srv.GET("/documents").DataList(); len(items) != 0 {
		t.Errorf("got %d records, want 0", len(items))
	}
}

// The ceiling bounds the request, it does not break ordinary uploads under it.
func TestUploadLimit_UploadUnderLimitStillWorks(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := uploadLimitServer(t, store, 64<<10)

	srv.POSTMultipart("/documents", map[string]string{"title": "small"}, uploadOf(8<<10)).
		AssertStatus(http.StatusCreated)

	if keys := store.Keys(); len(keys) != 1 {
		t.Errorf("stored %d objects, want 1", len(keys))
	}
}

// The standalone POST /files endpoint parses multipart on its own path and needs
// the same ceiling.
func TestUploadLimit_OversizedStandaloneUploadRejected(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := uploadLimitServer(t, store, 64<<10)

	resp := srv.POSTMultipart("/files", nil, uploadOf(256<<10))
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "BODY_TOO_LARGE" {
		t.Errorf("error code: got %q, want BODY_TOO_LARGE", code)
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Errorf("stored %d objects, want 0", len(keys))
	}
}

// A per-model body.MaxBodySize tightens the multipart ceiling too — one knob for
// "how big may this request be", whatever the content type.
func TestUploadLimit_MaxBodySizeOverridesUploadCeiling(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := uploadLimitServer(t, store, 1<<20, body.MaxBodySize(16<<10)) // 1 MB app-wide, 16 KB here

	resp := srv.POSTMultipart("/documents", map[string]string{"title": "big"}, uploadOf(64<<10))
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "BODY_TOO_LARGE" {
		t.Errorf("error code: got %q, want BODY_TOO_LARGE", code)
	}
}
