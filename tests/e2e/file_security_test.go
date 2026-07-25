package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const storagePrivateError = `s3: bucket=private-payments endpoint=http://minio.internal access_key=secret`

type failingRetrieveStorage struct {
	*testutil.MemoryStorage
}

func (s *failingRetrieveStorage) Retrieve(context.Context, string) (io.ReadCloser, maniflex.FileMeta, error) {
	return nil, maniflex.FileMeta{}, errors.New(storagePrivateError)
}

// SEC-4a: mounting the standalone /files endpoints without any BeforeMiddlewares
// leaves upload/download/delete open to anyone. The framework must warn loudly at
// boot (and stay quiet when auth is configured).
func TestFilesMount_WarnsWhenUnauthenticated(t *testing.T) {
	t.Parallel()

	buildWith := func(before ...maniflex.MiddlewareFunc) string {
		var buf bytes.Buffer
		srv := maniflex.New(maniflex.Config{
			Logger: slog.New(slog.NewTextHandler(&buf, nil)),
			FilesConfig: maniflex.FilesConfig{
				Storage:           testutil.NewMemoryStorage(),
				MountEndpoints:    true,
				BeforeMiddlewares: before,
			},
		})
		_ = srv.Handler() // builds the router → emits the SEC-4 warning if unauth
		return buf.String()
	}

	if logs := buildWith(); !strings.Contains(logs, "without auth middleware") {
		t.Errorf("expected an unauthenticated-/files warning, got logs:\n%s", logs)
	}

	authed := buildWith(func(ctx *maniflex.ServerContext, next func() error) error { return next() })
	if strings.Contains(authed, "without auth middleware") {
		t.Errorf("did not expect a warning when BeforeMiddlewares is set, got logs:\n%s", authed)
	}
}

// SEC-4b: a stored file must not be able to run as script on the API origin.
// writeFileResponse always sends X-Content-Type-Options: nosniff and only serves
// an allowlist of content types inline; everything else is forced to download.
func TestFileServe_StoredXSSNeutralized(t *testing.T) {
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := fileServer(t, store)

	serveType := func(t *testing.T, filename, contentType string, body []byte) *testutil.Response {
		t.Helper()
		up := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: filename, ContentType: contentType, Body: body},
		})
		up.AssertStatus(http.StatusCreated)
		return srv.GETRaw("/files/" + testutil.Field(t, up.Data(), "key"))
	}

	// Dangerous types must download (attachment), never render inline.
	for _, tc := range []struct {
		name, filename, contentType string
		body                        []byte
	}{
		{"html", "xss.html", "text/html", []byte(`<script>alert(document.domain)</script>`)},
		{"svg", "xss.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
	} {
		t.Run(tc.name+"_downloads", func(t *testing.T) {
			resp := serveType(t, tc.filename, tc.contentType, tc.body)
			resp.AssertStatus(http.StatusOK)
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
			}
			if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
				t.Errorf("%s served with Content-Disposition %q; want attachment (stored XSS)", tc.contentType, cd)
			}
		})
	}

	// A safe image is still served inline (with nosniff) for in-browser viewing.
	t.Run("png_inline", func(t *testing.T) {
		resp := serveType(t, "pic.png", "image/png", fakePNG)
		resp.AssertStatus(http.StatusOK)
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
			t.Errorf("image/png served with Content-Disposition %q; want inline", cd)
		}
	})
}

func TestFileServe_RedactsStorageFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	store := &failingRetrieveStorage{MemoryStorage: testutil.NewMemoryStorage()}
	srv := testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: store,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
	})

	resp := srv.GETRaw("/files/private-key")
	resp.AssertStatus(http.StatusInternalServerError)
	assertRedactedServerError(t, resp, storagePrivateError)
	if resp.ErrorCode() != "RETRIEVE_ERROR" {
		t.Errorf("error code = %q, want RETRIEVE_ERROR", resp.ErrorCode())
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("500 response is missing X-Request-Id correlation header")
	}
	if got := logs.String(); !strings.Contains(got, storagePrivateError) ||
		!strings.Contains(got, "request_id=") {
		t.Errorf("private storage diagnostic and request ID must be logged; got:\n%s", got)
	}
}
