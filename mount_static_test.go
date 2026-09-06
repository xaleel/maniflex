package maniflex

// Audit HTTP-11 — Mount forwards PathPrefix and nothing else, while the static
// tree is the one route Handler registers outside PathPrefix (so assets keep
// short URLs). A mounted server therefore 404s every file it serves standalone,
// at no prefix, with nothing in the log to notice it by.
//
// The mount stays as it is — auto-mounting the tree would panic on a second
// service with StaticDir set, which is the shape Mount exists for. What changes
// is that it says so.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type mountStaticModel struct {
	BaseModel
	Title string `json:"title"`
}

// mountedServer builds a server with a static directory and mounts it on an
// outer chi router, returning the warnings Mount emitted.
func mountedServer(t *testing.T, cfg Config) (*chi.Mux, []map[string]any) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg.StaticDir == "-" {
		cfg.StaticDir = ""
	} else {
		cfg.StaticDir = dir
	}

	var logBuf bytes.Buffer
	cfg.PathPrefix = "/api"
	cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	srv := New(cfg)
	srv.MustRegister(mountStaticModel{})

	r := chi.NewRouter()
	Mount(r, srv)

	var warnings []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if msg, _ := entry["msg"].(string); strings.Contains(msg, "StaticDir is unreachable") {
			warnings = append(warnings, entry)
		}
	}
	return r, warnings
}

func TestMount_WarnsThatStaticIsUnreachable(t *testing.T) {
	t.Parallel()

	r, warnings := mountedServer(t, Config{})
	if len(warnings) != 1 {
		t.Fatalf("Mount emitted %d warnings about static, want 1", len(warnings))
	}
	w := warnings[0]
	if got := w["static_prefix"]; got != "/static" {
		t.Errorf("static_prefix = %v, want /static — the warning has to name the dark path", got)
	}
	if got := w["path_prefix"]; got != "/api" {
		t.Errorf("path_prefix = %v, want /api", got)
	}

	// The condition the warning describes must actually hold.
	ts := httptest.NewServer(r)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /static/app.js through Mount → %d; the warning would be false", resp.StatusCode)
	}
}

// Nothing to warn about when no static tree would have been mounted: the two
// cases are exactly mountStatic's own guard, so the warning cannot fire for a
// server that was never going to serve files.
func TestMount_StaysQuietWhenNoStaticIsServed(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]Config{
		"no StaticDir":   {StaticDir: "-"},
		"StaticDisabled": {StaticDisabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, warnings := mountedServer(t, cfg); len(warnings) != 0 {
				t.Errorf("Mount warned about static for %s: %v", name, warnings)
			}
		})
	}
}

// The documented way out, pinned so the docs cannot drift from what works:
// forwarding to the same handler with a fresh RouteContext, which reuses the
// framework's own hardened static handler rather than an http.FileServer.
func TestMount_StaticReachableWhenForwardedOnTheOuterRouter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{PathPrefix: "/api", StaticDir: dir})
	srv.MustRegister(mountStaticModel{})

	r := chi.NewRouter()
	Mount(r, srv)
	inner := srv.Handler()
	r.Handle("/static/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, chi.NewRouteContext()))
		inner.ServeHTTP(w, req)
	}))

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 32)
	n, _ := resp.Body.Read(body)
	if resp.StatusCode != http.StatusOK || string(body[:n]) != "console.log(1)" {
		t.Fatalf("forwarded GET /static/app.js → %d %q", resp.StatusCode, body[:n])
	}
}
