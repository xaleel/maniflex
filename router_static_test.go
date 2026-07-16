package maniflex_test

// Static file serving is configurable (§10.3): StaticDir sets the filesystem
// directory, StaticPrefix the URL prefix, and StaticDisabled turns it off. These
// drive the real router via Server.Handler(); no DB or models are required.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xaleel/maniflex"
)

// writeStaticFile creates dir/name with the given contents and returns dir.
func writeStaticFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write static file: %v", err)
	}
	return dir
}

// getStatic issues a bare GET against the handler (root-relative, not under the
// API prefix) and returns status + body.
func getStatic(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func TestStatic_CustomDirAndPrefix(t *testing.T) {
	dir := writeStaticFile(t, "hello.txt", "custom-static-body")

	srv := maniflex.New(maniflex.Config{
		StaticDir:    dir,
		StaticPrefix: "/assets",
	})
	h := srv.Handler()

	if code, body := getStatic(t, h, "/assets/hello.txt"); code != http.StatusOK || body != "custom-static-body" {
		t.Errorf("GET /assets/hello.txt = %d %q, want 200 \"custom-static-body\"", code, body)
	}
	// The default /static prefix is no longer mounted when a custom one is set.
	if code, _ := getStatic(t, h, "/static/hello.txt"); code != http.StatusNotFound {
		t.Errorf("GET /static/hello.txt = %d, want 404 (custom prefix replaced the default)", code)
	}
}

// StaticPrefix defaults to /static when unset, served from the custom dir.
func TestStatic_DefaultPrefix(t *testing.T) {
	dir := writeStaticFile(t, "app.js", "console.log(1)")

	h := maniflex.New(maniflex.Config{StaticDir: dir}).Handler()

	if code, body := getStatic(t, h, "/static/app.js"); code != http.StatusOK || body != "console.log(1)" {
		t.Errorf("GET /static/app.js = %d %q, want 200 with body", code, body)
	}
}

// A prefix without a leading slash is normalised instead of panicking chi.
func TestStatic_PrefixWithoutLeadingSlash(t *testing.T) {
	dir := writeStaticFile(t, "x.css", "body{}")

	h := maniflex.New(maniflex.Config{StaticDir: dir, StaticPrefix: "assets"}).Handler()

	if code, body := getStatic(t, h, "/assets/x.css"); code != http.StatusOK || body != "body{}" {
		t.Errorf("GET /assets/x.css = %d %q, want 200 \"body{}\"", code, body)
	}
}

// StaticDisabled turns serving off even when the directory exists.
func TestStatic_Disabled(t *testing.T) {
	dir := writeStaticFile(t, "secret.txt", "nope")

	h := maniflex.New(maniflex.Config{
		StaticDir:      dir,
		StaticPrefix:   "/static",
		StaticDisabled: true,
	}).Handler()

	if code, _ := getStatic(t, h, "/static/secret.txt"); code != http.StatusNotFound {
		t.Errorf("GET /static/secret.txt = %d, want 404 when StaticDisabled", code)
	}
}

// A missing directory is skipped without serving (and without panicking).
func TestStatic_MissingDirSkipped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	h := maniflex.New(maniflex.Config{StaticDir: missing, StaticPrefix: "/static"}).Handler()

	if code, _ := getStatic(t, h, "/static/anything.txt"); code != http.StatusNotFound {
		t.Errorf("GET against missing static dir = %d, want 404", code)
	}
}

// writeTree writes each path→content pair under a fresh temp dir, creating parent
// directories, and returns the dir. Paths are slash-separated, relative to it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// Static serving is now opt-in: an empty StaticDir mounts nothing. It used to
// fall back to "<cwd>/static", so a static/ directory that merely happened to be
// in the working tree was published at /static/ unasked (DX-6). Put one there and
// prove the fallback is gone.
func TestStatic_UnsetStaticDirServesNothing(t *testing.T) {
	work := writeTree(t, map[string]string{
		"static/secret.txt": "TOP SECRET",
	})
	t.Chdir(work)

	h := maniflex.New(maniflex.Config{}).Handler() // StaticDir unset

	if code, body := getStatic(t, h, "/static/secret.txt"); code != http.StatusNotFound {
		t.Errorf("GET /static/secret.txt = %d %q, want 404 — a static/ dir in the "+
			"working tree must no longer be published without opting in", code, body)
	}
}

// The serving contract the opt-in preserves: a lone file is served at its path,
// and a full SPA under a subdirectory is served in full, nested assets included.
func TestStatic_ServesFileAndNestedSPA(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"test.json":                     `{"ok":true}`,
		"frontend-app/index.html":       "<!doctype html>APP",
		"frontend-app/scripts/index.js": "console.log('spa')",
	})

	h := maniflex.New(maniflex.Config{StaticDir: dir}).Handler()

	cases := []struct{ path, want string }{
		{"/static/test.json", `{"ok":true}`},                            // a single top-level file
		{"/static/frontend-app/", "<!doctype html>APP"},                 // dir with index.html → the SPA
		{"/static/frontend-app/scripts/index.js", "console.log('spa')"}, // a nested asset
	}
	for _, tc := range cases {
		if code, body := getStatic(t, h, tc.path); code != http.StatusOK || body != tc.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tc.path, code, body, tc.want)
		}
	}

	// The SPA path without its trailing slash redirects onto it, as http.FileServer
	// does — so /static/frontend-app reaches the app too.
	if code, _ := getStatic(t, h, "/static/frontend-app"); code != http.StatusOK {
		// getStatic follows redirects, so the 301→/ lands on index.html: 200.
		t.Errorf("GET /static/frontend-app = %d, want 200 after the trailing-slash redirect", code)
	}
}
