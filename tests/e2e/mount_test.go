package e2e_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"maniflex"
	"maniflex/db/sqlite"
)

type mountModelA struct{ maniflex.BaseModel }
type mountModelB struct{ maniflex.BaseModel }

// newMountServer creates a minimal Server instance with an in-memory SQLite DB.
func newMountServer(t *testing.T, prefix string, models ...any) *maniflex.Server {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: prefix})
	srv.MustRegister(models...)
	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("newMountServer: sqlite.Open: %v", err)
	}
	if err := db.AutoMigrate(context.Background(), srv.Registry()); err != nil {
		t.Fatalf("newMountServer: AutoMigrate: %v", err)
	}
	srv.SetDB(db)
	t.Cleanup(func() { db.Close() })
	return srv
}

func TestMount_EachServiceReceivesRequests(t *testing.T) {
	srvA := newMountServer(t, "/api/a", mountModelA{})
	srvB := newMountServer(t, "/api/b", mountModelB{})

	r := chi.NewRouter()
	maniflex.Mount(r, srvA, srvB)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/a/mount_model_as")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/a/mount_model_as: want 200, got %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/b/mount_model_bs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/b/mount_model_bs: want 200, got %d", resp.StatusCode)
	}
}

func TestMount_CrossPrefixRoutesReturn404(t *testing.T) {
	srvA := newMountServer(t, "/api/a", mountModelA{})
	srvB := newMountServer(t, "/api/b", mountModelB{})

	r := chi.NewRouter()
	maniflex.Mount(r, srvA, srvB)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	// Service B does not have mountModelA routes.
	resp, err := http.Get(ts.URL + "/api/b/mount_model_as")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /api/b/mount_model_as: expected non-200, got 200")
	}
}

func TestMount_SharedMiddlewareRunsOnce(t *testing.T) {
	srvA := newMountServer(t, "/api/a", mountModelA{})

	hits := 0
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			hits++
			next.ServeHTTP(w, req)
		})
	})
	maniflex.Mount(r, srvA)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/a/mount_model_as")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if hits != 1 {
		t.Errorf("shared middleware: want 1 call, got %d", hits)
	}
}
