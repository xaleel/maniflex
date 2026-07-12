package e2e

// The OpenAPI generator must read the Config the server actually serves from.
// It used to hold a pointer to New's local copy while SetStorage/SetDB mutated
// the server's own copy, so the documented two-step init produced a spec that
// omitted the file routes the router had really mounted (BUG-10).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// twoStepServer builds a server the documented way: New with a bare Config, then
// inject the adapter and storage afterwards.
func twoStepServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	srv.MustRegister(testutil.Document{})

	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	srv.SetDB(db)
	srv.SetStorage(testutil.NewMemoryStorage())

	// Handler() does not migrate — only Start() does — so create the table here.
	if err := db.AutoMigrate(context.Background(), srv.Registry()); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// fetchSpec returns the decoded /openapi.json document.
func fetchSpec(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	return spec
}

func TestOpenAPI_ReflectsSetStorage(t *testing.T) {
	t.Parallel()
	ts := twoStepServer(t)

	paths, ok := fetchSpec(t, ts)["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths object")
	}

	// Storage was set after New, so the attachment routes the router mounts must
	// be in the spec too — one per mfx:"file" field on Document.
	for _, want := range []string{"/documents/{id}/file", "/documents/{id}/icon"} {
		if _, found := paths[want]; !found {
			t.Errorf("spec is missing %q — SetStorage was not reflected in the generated document", want)
		}
	}
}

// The routes the spec now advertises are the ones actually served: a request to
// the attachment path reaches the pipeline (404 for a missing record) rather
// than falling through the router as an unmounted path.
func TestOpenAPI_AdvertisedAttachmentRouteIsMounted(t *testing.T) {
	t.Parallel()
	ts := twoStepServer(t)

	resp, err := ts.Client().Get(ts.URL + "/api/documents/no-such-id/file")
	if err != nil {
		t.Fatalf("GET attachment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 from the mounted attachment route", resp.StatusCode)
	}
}
