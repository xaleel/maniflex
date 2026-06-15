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

	"maniflex"
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
