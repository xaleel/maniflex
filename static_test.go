package maniflex

// Audit HTTP-9 — the static mount was chi's Handle over http.FileServer, which
// answered every method with the file's contents, served dotfiles, listed
// directories that had no index.html, and followed symlinks out of the root. A
// StaticDir that was also a working tree published .env, .git/config, its own
// file index, and whatever a link pointed at.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type staticTestModel struct {
	BaseModel
	Title string `json:"title"`
}

// staticTree builds a directory with the shapes that matter: an ordinary asset,
// the dotfiles a working tree leaves lying around, a .well-known challenge, a
// subdirectory with an index and one without.
func staticTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("app.js", "console.log(1)")
	write(".env", "DB_PASSWORD=hunter2")
	write(".git/config", "[remote]")
	write(".well-known/acme-challenge/token", "challenge-response")
	write("admin/index.html", "<h1>admin</h1>")
	write("assets/logo.svg", "<svg/>")
	return dir
}

func staticTestServer(t *testing.T, dir string, listing bool) *httptest.Server {
	t.Helper()
	srv := New(Config{StaticDir: dir, StaticDirectoryListing: listing})
	srv.MustRegister(staticTestModel{})
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func staticReq(t *testing.T, ts *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Following a redirect would hide which status the mount itself answered.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Only GET and HEAD have a meaning here. A method that reads as a write must not
// come back looking like it worked.
func TestStatic_OnlyGetAndHead(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	if status, body := staticReq(t, ts, http.MethodGet, "/static/app.js"); status != http.StatusOK || body == "" {
		t.Fatalf("GET → %d %q, want the file", status, body)
	}
	if status, _ := staticReq(t, ts, http.MethodHead, "/static/app.js"); status != http.StatusOK {
		t.Errorf("HEAD → %d, want 200", status)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		status, body := staticReq(t, ts, m, "/static/app.js")
		if status == http.StatusOK {
			t.Errorf("%s /static/app.js → 200 with %d bytes; only GET and HEAD are served",
				m, len(body))
		}
	}
}

func TestStatic_DotfilesAreRefused(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	for _, p := range []string{
		"/static/.env",
		"/static/.git/config",
		"/static/.git/",
	} {
		status, body := staticReq(t, ts, http.MethodGet, p)
		if status != http.StatusNotFound {
			t.Errorf("GET %s → %d %q, want 404", p, status, body)
		}
	}
}

// The exception, and the reason it exists: refusing it stops ACME renewal, which
// surfaces weeks later as an expired certificate rather than as a visible 404.
func TestStatic_WellKnownIsServed(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	status, body := staticReq(t, ts, http.MethodGet, "/static/.well-known/acme-challenge/token")
	if status != http.StatusOK || body != "challenge-response" {
		t.Errorf("GET the ACME challenge → %d %q, want 200 and the token", status, body)
	}
}

func TestStatic_DirectoryWithoutIndexIs404ByDefault(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	status, body := staticReq(t, ts, http.MethodGet, "/static/assets/")
	if status != http.StatusNotFound {
		t.Errorf("GET /static/assets/ → %d, want 404; a listing names files nothing links to", status)
	}
	if strings.Contains(body, "logo.svg") {
		t.Error("the response names a file in the directory")
	}
}

func TestStatic_DirectoryWithIndexServesIt(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	status, body := staticReq(t, ts, http.MethodGet, "/static/admin/")
	if status != http.StatusOK || !strings.Contains(body, "<h1>admin</h1>") {
		t.Errorf("GET /static/admin/ → %d %q, want its index.html", status, body)
	}
}

func TestStatic_ListingIsOptIn(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), true)

	status, body := staticReq(t, ts, http.MethodGet, "/static/assets/")
	if status != http.StatusOK || !strings.Contains(body, "logo.svg") {
		t.Fatalf("GET /static/assets/ with listings on → %d %q", status, body)
	}
	// A listing must not advertise what the handler would refuse.
	root, rootBody := staticReq(t, ts, http.MethodGet, "/static/")
	if root != http.StatusOK {
		t.Fatalf("GET /static/ → %d", root)
	}
	for _, hidden := range []string{".env", ".git"} {
		if strings.Contains(rootBody, hidden) {
			t.Errorf("the listing names %q, which the handler refuses to serve", hidden)
		}
	}
}

// os.Root resolves every component inside the directory, so a link under it
// cannot read outside it.
func TestStatic_SymlinkOutOfRootIsRefused(t *testing.T) {
	t.Parallel()
	dir := staticTree(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the static root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	ts := staticTestServer(t, dir, false)

	status, body := staticReq(t, ts, http.MethodGet, "/static/link.txt")
	if status != http.StatusNotFound {
		t.Errorf("GET /static/link.txt → %d %q, want 404", status, body)
	}
}

// A relative symlink that stays inside the root is an ordinary file and keeps
// working — the ordinary kind within an asset tree.
func TestStatic_SymlinkInsideRootIsServed(t *testing.T) {
	t.Parallel()
	dir := staticTree(t)
	if err := os.Symlink("app.js", filepath.Join(dir, "alias.js")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	ts := staticTestServer(t, dir, false)

	if status, body := staticReq(t, ts, http.MethodGet, "/static/alias.js"); status != http.StatusOK ||
		body != "console.log(1)" {
		t.Errorf("GET /static/alias.js → %d %q, want the linked file", status, body)
	}
}

// An absolute target is refused even when it resolves inside the root: os.Root
// cannot confirm that without resolving outside its own walk, which is the race
// it exists to remove. Pinned here so the limitation is a decision, not a
// surprise.
func TestStatic_AbsoluteSymlinkIsRefusedEvenInsideRoot(t *testing.T) {
	t.Parallel()
	dir := staticTree(t)
	if err := os.Symlink(filepath.Join(dir, "app.js"), filepath.Join(dir, "abs.js")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	ts := staticTestServer(t, dir, false)

	if status, _ := staticReq(t, ts, http.MethodGet, "/static/abs.js"); status != http.StatusNotFound {
		t.Errorf("GET /static/abs.js → %d, want 404", status)
	}
}

func TestStatic_TraversalIsRefused(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	for _, p := range []string{
		"/static/../router.go",
		"/static/assets/../../router.go",
		"/static/%2e%2e/router.go",
	} {
		if status, _ := staticReq(t, ts, http.MethodGet, p); status == http.StatusOK {
			t.Errorf("GET %s → 200", p)
		}
	}
}

// Range and conditional requests are what http.ServeContent buys, and a video or
// a large bundle depends on them.
func TestStatic_RangeRequestsWork(t *testing.T) {
	t.Parallel()
	ts := staticTestServer(t, staticTree(t), false)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/static/app.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-6")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "console" {
		t.Errorf("range request → %d %q, want 206 and the first 7 bytes", resp.StatusCode, body)
	}
}

func TestStaticFilePath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"/", ".", true},
		{"/app.js", "app.js", true},
		{"/a/b/c.txt", "a/b/c.txt", true},
		{"/.well-known/x", ".well-known/x", true},
		{"/.env", "", false},
		{"/a/.git/config", "", false},
		{"/../etc/passwd", "etc/passwd", true}, // cleaned; os.Root contains the rest
	} {
		got, ok := staticFilePath(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("staticFilePath(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
