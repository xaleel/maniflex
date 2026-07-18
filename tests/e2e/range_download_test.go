package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// rangeBody is the object every case below slices. It is deliberately long
// enough that a window is unmistakably a window, and its bytes encode their own
// offset so a wrong window is obvious in the failure message.
var rangeBody = func() []byte {
	b := make([]byte, 1000)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}()

// noRangeStorage wraps a MemoryStorage but exposes ONLY maniflex.FileStorage —
// no RetrieveRange, and its reader is not seekable. It stands in for a
// third-party backend that has not adopted the optional interface, which must
// keep working (Range ignored, whole object, 200).
type noRangeStorage struct{ inner *testutil.MemoryStorage }

func (s noRangeStorage) Store(ctx context.Context, key string, r io.Reader, m maniflex.FileMeta) error {
	return s.inner.Store(ctx, key, r, m)
}

func (s noRangeStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, maniflex.FileMeta, error) {
	rc, m, err := s.inner.Retrieve(ctx, key)
	if err != nil {
		return nil, m, err
	}
	// Hide any Seek the inner reader might offer, so this really is the
	// bottom tier and not the ServeContent fallback in disguise.
	return readCloserOnly{rc}, m, nil
}

func (s noRangeStorage) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, key)
}
func (s noRangeStorage) Exists(ctx context.Context, key string) (bool, error) {
	return s.inner.Exists(ctx, key)
}
func (s noRangeStorage) Stat(ctx context.Context, key string) (maniflex.FileMeta, error) {
	return s.inner.Stat(ctx, key)
}
func (s noRangeStorage) PresignUpload(ctx context.Context, key string,
	o maniflex.PresignUploadOptions,
) (*maniflex.PresignedUpload, error) {
	return s.inner.PresignUpload(ctx, key, o)
}
func (s noRangeStorage) URL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.inner.URL(ctx, key, ttl)
}

// readCloserOnly strips every method but Read and Close.
type readCloserOnly struct{ rc io.ReadCloser }

func (r readCloserOnly) Read(p []byte) (int, error) { return r.rc.Read(p) }
func (r readCloserOnly) Close() error               { return r.rc.Close() }

// assertPartial checks a 206 carries exactly the requested window.
func assertPartial(t *testing.T, resp *testutil.Response, start, end int64) {
	t.Helper()
	resp.AssertStatus(http.StatusPartialContent)
	wantCR := fmt.Sprintf("bytes %d-%d/%d", start, end, len(rangeBody))
	if got := resp.Header.Get("Content-Range"); got != wantCR {
		t.Errorf("Content-Range: got %q want %q", got, wantCR)
	}
	wantLen := fmt.Sprintf("%d", end-start+1)
	if got := resp.Header.Get("Content-Length"); got != wantLen {
		t.Errorf("Content-Length: got %q want %q", got, wantLen)
	}
	want := rangeBody[start : end+1]
	if string(resp.Body) != string(want) {
		t.Errorf("body: got %d bytes %q…, want %d bytes %q…",
			len(resp.Body), truncate(resp.Body), len(want), truncate(want))
	}
}

func truncate(b []byte) string {
	if len(b) > 20 {
		return string(b[:20])
	}
	return string(b)
}

// TestRangeDownload covers roadmap item 3B.3b — HTTP Range support for
// resumable downloads, on both download routes.
//
//	go test ./tests/e2e/... -run TestRangeDownload
func TestRangeDownload(t *testing.T) {
	t.Parallel()

	// uploadToFiles puts rangeBody in storage and returns its /files/* path.
	uploadToFiles := func(t *testing.T, srv *testutil.Server) string {
		t.Helper()
		up := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "clip.bin", ContentType: "application/octet-stream", Body: rangeBody},
		})
		up.AssertStatus(http.StatusCreated)
		return "/files/" + testutil.Field(t, up.Data(), "key")
	}

	t.Run("files_route_serves_206_window", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())
		path := uploadToFiles(t, srv)

		resp := srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=100-199"})
		assertPartial(t, resp, 100, 199)
	})

	t.Run("attachment_route_serves_206_window", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())

		create := srv.POSTMultipart("/documents", map[string]string{"title": "Doc"},
			map[string]testutil.FileUpload{
				"file": {Filename: "clip.pdf", ContentType: "application/pdf", Body: rangeBody},
			})
		create.AssertStatus(http.StatusCreated)

		resp := srv.GETRawWithHeaders("/documents/"+create.ID()+"/file",
			map[string]string{"Range": "bytes=0-9"})
		assertPartial(t, resp, 0, 9)
		// The security headers the full-body path sets must survive onto the 206.
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("206 dropped X-Content-Type-Options: nosniff")
		}
		if resp.Header.Get("Content-Disposition") == "" {
			t.Error("206 dropped Content-Disposition")
		}
	})

	t.Run("suffix_and_open_ended_windows", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())
		path := uploadToFiles(t, srv)

		// "the last 50 bytes" — what a resuming client that lost the tail asks for.
		assertPartial(t, srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=-50"}), 950, 999)
		// "everything from byte 900 on" — what a resuming download asks for.
		assertPartial(t, srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=900-"}), 900, 999)
	})

	t.Run("full_request_advertises_accept_ranges", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())
		path := uploadToFiles(t, srv)

		resp := srv.GETRaw(path)
		resp.AssertStatus(http.StatusOK)
		if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges: got %q want \"bytes\" — a client cannot resume what is not advertised", got)
		}
		if string(resp.Body) != string(rangeBody) {
			t.Errorf("full body: got %d bytes want %d", len(resp.Body), len(rangeBody))
		}
	})

	t.Run("out_of_range_is_416", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())
		path := uploadToFiles(t, srv)

		resp := srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=5000-6000"})
		resp.AssertStatus(http.StatusRequestedRangeNotSatisfiable)
		if code := jsonErrorCode(resp.Body); code != "RANGE_NOT_SATISFIABLE" {
			t.Errorf("error code: got %q want RANGE_NOT_SATISFIABLE", code)
		}
		// The 416 must tell the client how long the object actually is.
		wantCR := fmt.Sprintf("bytes */%d", len(rangeBody))
		if got := resp.Header.Get("Content-Range"); got != wantCR {
			t.Errorf("Content-Range: got %q want %q", got, wantCR)
		}
	})

	t.Run("multi_range_falls_back_to_200", func(t *testing.T) {
		t.Parallel()
		srv := fileServer(t, testutil.NewMemoryStorage())
		path := uploadToFiles(t, srv)

		// Serving several windows needs a multipart/byteranges body; declining
		// to and sending the whole object is allowed, and must not be a 416.
		resp := srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=0-99,200-299"})
		resp.AssertStatus(http.StatusOK)
		if string(resp.Body) != string(rangeBody) {
			t.Errorf("multi-range fallback: got %d bytes want the whole %d", len(resp.Body), len(rangeBody))
		}
	})

	t.Run("backend_without_range_support_still_serves_200", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: noRangeStorage{inner: testutil.NewMemoryStorage()},
		})
		path := uploadToFiles(t, srv)

		resp := srv.GETRawWithHeaders(path, map[string]string{"Range": "bytes=100-199"})
		resp.AssertStatus(http.StatusOK)
		if string(resp.Body) != string(rangeBody) {
			t.Errorf("non-range backend: got %d bytes want the whole %d", len(resp.Body), len(rangeBody))
		}
		// And it must not claim a capability it does not have.
		if got := resp.Header.Get("Accept-Ranges"); got != "" {
			t.Errorf("Accept-Ranges: got %q, want unset — the backend cannot serve ranges", got)
		}
	})
}
