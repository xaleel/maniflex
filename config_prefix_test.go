package maniflex

// Audit HTTP-12 — Config filled in empty values and normalised nothing, so a
// PathPrefix reached chi verbatim: "api" panicked out of buildRouter, from
// inside Start(), after validation had reported no problem. "//api" was the
// quieter one — no panic, every route a doubled slash deep, and nothing sends
// that, so the whole API answered 404.
//
// Port and StaticPrefix failed the same way by other routes: a port out of range
// only reached net.Listen, which runs after migration and after services have
// started, and a StaticPrefix carrying { } or * panicked in fileServer. Neither
// can be guessed at, so both are startup issues now — reported with everything
// else, before any side effect.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type prefixModel struct {
	BaseModel
	Title string `json:"title"`
}

func TestNormalisePrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"/api", "/api"},
		{"api", "/api"},
		{"/api/", "/api"},
		{"api/", "/api"},
		{"//api", "/api"},
		{"/api/v1", "/api/v1"},
		{"api/v1/", "/api/v1"},
		{"/", "/"},
		{"//", "/"},
		{"", "/"},
	} {
		if got := normalisePrefix(tc.in); got != tc.want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The shapes that used to panic or route into the void now serve the same API as
// the canonical spelling.
func TestConfig_PathPrefixIsNormalised(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"/api", "api", "/api/", "//api"} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()

			srv := New(Config{PathPrefix: prefix})
			srv.MustRegister(prefixModel{})
			if got := srv.cfg.PathPrefix; got != "/api" {
				t.Fatalf("Config.PathPrefix = %q, want /api", got)
			}

			h, err := srv.handler()
			if err != nil {
				t.Fatalf("handler(): %v", err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/live", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET /api/live → %d, want 200", rec.Code)
			}
		})
	}
}

// "/" is a prefix, not an empty one: it must survive normalisation and keep
// mounting the API at the router root.
func TestConfig_RootPathPrefixSurvives(t *testing.T) {
	t.Parallel()

	srv := New(Config{PathPrefix: "/"})
	srv.MustRegister(prefixModel{})
	if got := srv.cfg.PathPrefix; got != "/" {
		t.Fatalf("Config.PathPrefix = %q, want /", got)
	}
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /live → %d, want 200", rec.Code)
	}
}

func TestConfig_PortOutOfRangeIsRefusedAtStartup(t *testing.T) {
	t.Parallel()

	for _, port := range []int{-1, 65536, 99999} {
		srv := New(Config{Port: port})
		srv.MustRegister(prefixModel{})

		_, err := srv.handler()
		if err == nil {
			t.Fatalf("Port %d built a router; net.Listen would have refused it only "+
				"after migration and service start", port)
		}
		if !strings.Contains(err.Error(), "Config.Port") {
			t.Errorf("Port %d → %v, want an error naming Config.Port", port, err)
		}
	}

	// The default still applies: zero means 8080, not "out of range".
	srv := New(Config{})
	srv.MustRegister(prefixModel{})
	if _, err := srv.handler(); err != nil {
		t.Fatalf("default port rejected: %v", err)
	}
}

func TestConfig_StaticPrefixRoutingCharactersAreRefusedAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"/as{sets", "/assets*", "/assets/{id}"} {
		srv := New(Config{StaticDir: dir, StaticPrefix: prefix})
		srv.MustRegister(prefixModel{})

		_, err := srv.handler()
		if err == nil {
			t.Errorf("StaticPrefix %q built a router; fileServer panics on it", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "Config.StaticPrefix") {
			t.Errorf("StaticPrefix %q → %v, want an error naming Config.StaticPrefix", prefix, err)
		}
	}

	// Nothing to refuse when no static tree is mounted, even with a broken prefix.
	srv := New(Config{StaticPrefix: "/as{sets"})
	srv.MustRegister(prefixModel{})
	if _, err := srv.handler(); err != nil {
		t.Errorf("StaticPrefix rejected with no StaticDir set: %v", err)
	}
}

// A bare static prefix mounts rather than panicking, and normalisation reaches
// the mount itself.
func TestConfig_StaticPrefixIsNormalised(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"/assets", "assets", "/assets/"} {
		srv := New(Config{StaticDir: dir, StaticPrefix: prefix})
		srv.MustRegister(prefixModel{})
		h, err := srv.handler()
		if err != nil {
			t.Fatalf("StaticPrefix %q: handler(): %v", prefix, err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
			t.Errorf("StaticPrefix %q: GET /assets/app.js → %d %q", prefix, rec.Code, rec.Body.String())
		}
	}
}
