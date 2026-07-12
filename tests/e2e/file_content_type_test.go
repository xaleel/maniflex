package e2e

// An uploaded part's content type comes from what the client declared. Sniffing
// is only a fallback: http.DetectContentType knows a short list of magic numbers
// and answers text/plain for everything else, so letting it override the declared
// header made mfx:"accept" unsatisfiable for JSON, CSV, and office formats — the
// client sent the right header and still got a 415.

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// typedDoc constrains each file field to a type that DetectContentType cannot
// recognise from its bytes (JSON and CSV both sniff as text/plain), plus one it
// can (PNG) to exercise the sniffing fallback.
type typedDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	Spec  string `json:"spec"  db:"spec"  mfx:"file,accept:application/json"`
	Sheet string `json:"sheet" db:"sheet" mfx:"file,accept:text/csv"`
	Image string `json:"image" db:"image" mfx:"file,accept:image/*"`
}

// pngBytes is a PNG magic-number header — DetectContentType reports image/png.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func typedDocServer(t *testing.T) (*testutil.Server, *testutil.MemoryStorage) {
	t.Helper()
	store := testutil.NewMemoryStorage()
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{typedDoc{}},
		FileStorage: store,
	}), store
}

// storedContentType returns the content type recorded for the record's file key.
func storedContentType(t *testing.T, store *testutil.MemoryStorage, key string) string {
	t.Helper()
	rc, meta, err := store.Retrieve(context.Background(), key)
	if err != nil {
		t.Fatalf("retrieve %q: %v", key, err)
	}
	rc.Close()
	return meta.ContentType
}

// The audit's scenario: valid JSON, correct header, accept:application/json —
// and a 415 every time, because the bytes sniffed as text/plain.
func TestFileContentType_DeclaredJSONAccepted(t *testing.T) {
	t.Parallel()
	srv, store := typedDocServer(t)

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"title": "spec"},
		map[string]testutil.FileUpload{
			"spec": {
				Filename:    "openapi.json",
				ContentType: "application/json",
				Body:        []byte(`{"openapi":"3.0.0"}`),
			},
		})
	resp.AssertStatus(http.StatusCreated)

	key, _ := resp.Data()["spec"].(string)
	if got := storedContentType(t, store, key); got != "application/json" {
		t.Errorf("stored content type = %q, want application/json", got)
	}
}

func TestFileContentType_DeclaredCSVAccepted(t *testing.T) {
	t.Parallel()
	srv, store := typedDocServer(t)

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"title": "rows"},
		map[string]testutil.FileUpload{
			"sheet": {
				Filename:    "rows.csv",
				ContentType: "text/csv",
				Body:        []byte("id,name\n1,alice\n"),
			},
		})
	resp.AssertStatus(http.StatusCreated)

	key, _ := resp.Data()["sheet"].(string)
	if got := storedContentType(t, store, key); got != "text/csv" {
		t.Errorf("stored content type = %q, want text/csv", got)
	}
}

// A part that declares nothing falls back to sniffing.
func TestFileContentType_MissingHeaderFallsBackToSniffing(t *testing.T) {
	t.Parallel()
	srv, store := typedDocServer(t)

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"title": "logo"},
		map[string]testutil.FileUpload{
			"image": {Filename: "logo.png", Body: pngBytes}, // no Content-Type
		})
	resp.AssertStatus(http.StatusCreated)

	key, _ := resp.Data()["image"].(string)
	if got := storedContentType(t, store, key); got != "image/png" {
		t.Errorf("stored content type = %q, want image/png (sniffed)", got)
	}
}

// So does the generic application/octet-stream clients send when they don't know
// the type — otherwise it would be taken at face value and fail accept:image/*.
func TestFileContentType_OctetStreamFallsBackToSniffing(t *testing.T) {
	t.Parallel()
	srv, store := typedDocServer(t)

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"title": "logo"},
		map[string]testutil.FileUpload{
			"image": {
				Filename:    "logo.png",
				ContentType: "application/octet-stream",
				Body:        pngBytes,
			},
		})
	resp.AssertStatus(http.StatusCreated)

	key, _ := resp.Data()["image"].(string)
	if got := storedContentType(t, store, key); got != "image/png" {
		t.Errorf("stored content type = %q, want image/png (sniffed)", got)
	}
}

// accept is still enforced — a declared type outside the list is rejected.
func TestFileContentType_DisallowedDeclaredTypeRejected(t *testing.T) {
	t.Parallel()
	srv, store := typedDocServer(t)

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"title": "evil"},
		map[string]testutil.FileUpload{
			"spec": {
				Filename:    "evil.html",
				ContentType: "text/html",
				Body:        []byte("<script>alert(1)</script>"),
			},
		})
	resp.AssertStatus(http.StatusUnsupportedMediaType)
	if code := resp.ErrorCode(); code != "FILE_TYPE_NOT_ALLOWED" {
		t.Errorf("error code: got %q, want FILE_TYPE_NOT_ALLOWED", code)
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Errorf("stored %d objects, want 0", len(keys))
	}
}
