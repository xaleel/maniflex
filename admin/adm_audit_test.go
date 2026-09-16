package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// Audit ADM-1 … ADM-6 — the admin panel's hardening pass.
//
//	go test ./admin/ -run TestADM

// HeadlessWidget registers fully but mounts no REST routes, so the panel has
// nothing to show for it (audit ADM-6).
type HeadlessWidget struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// admPanel mounts a panel over Widget plus a headless model. No DB adapter is
// set: every assertion here happens before the API call, or against a stub.
func admPanel(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Widget{})
	server.MustRegister(HeadlessWidget{}, maniflex.ModelConfig{Headless: true})
	cfg.AllowUnauthenticated = true
	return Mount(server, cfg)
}

// multipartBody builds a multipart form carrying one file part of size bytes.
func multipartBody(t *testing.T, field, filename string, size int) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return mw.FormDataContentType(), &buf
}

// ── ADM-1: the upload ceiling ────────────────────────────────────────────────

// TestADM1_OversizedSubmissionIsRefused is the bug. ParseMultipartForm's
// argument is the in-memory threshold, not a limit, so without a MaxBytesReader
// an authenticated user could post an unbounded body and the panel buffered and
// spooled it before the API's own limit was ever consulted.
func TestADM1_OversizedSubmissionIsRefused(t *testing.T) {
	h := admPanel(t, Config{MaxUploadBytes: 2048})

	ct, body := multipartBody(t, "file", "big.bin", 64<<10)
	req := httptest.NewRequest(http.MethodPost, "/admin/widgets", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized submission status = %d, want 413 — the body must be "+
			"refused before it is parsed", rec.Code)
	}
}

// TestADM1_NormalSubmissionPassesTheCeiling is the must-still-work half: a small
// body reaches the CSRF check, which is the next gate. A 403 here proves the
// size guard let it through; a 413 would mean the cap refuses ordinary forms.
func TestADM1_NormalSubmissionPassesTheCeiling(t *testing.T) {
	h := admPanel(t, Config{MaxUploadBytes: 1 << 20})

	req := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader("name=ok"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("an ordinary form body was refused as too large")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (CSRF, the gate after the size check)", rec.Code)
	}
}

// TestADM1_PartIsReferencedNotBuffered pins the streaming half: readUpload holds
// the multipart header, and writeMultipart opens it and copies the bytes through
// intact. It used to io.ReadAll the part and then copy that []byte into a second
// buffer, so the panel held the whole file twice.
func TestADM1_PartIsReferencedNotBuffered(t *testing.T) {
	const payload = "the-file-contents"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("doc", "a.txt")
	_, _ = part.Write([]byte(payload))
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/x", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("parse: %v", err)
	}

	uf := readUpload(r, "doc")
	if uf == nil {
		t.Fatal("readUpload returned nil for a present part")
	}
	if uf.Header == nil {
		t.Fatal("uploadedFile must carry the multipart header, not the bytes")
	}
	if uf.Header.Size != int64(len(payload)) {
		t.Errorf("part size = %d, want %d", uf.Header.Size, len(payload))
	}

	// Forward it and confirm the bytes arrive whole.
	rh := &recordingHandler{}
	c := &apiClient{handler: rh, apiPrefix: "/api"}
	if _, err := c.writeMultipart(nil, http.MethodPost, "/api/widgets",
		map[string]string{"name": "n"},
		map[string]*uploadedFile{"doc": uf}); err != nil {
		t.Fatalf("writeMultipart: %v", err)
	}
	if !bytes.Contains(rh.body, []byte(payload)) {
		t.Errorf("streamed part did not arrive intact:\n%s", rh.body)
	}
}

// ── ADM-2: the CSRF cookie ───────────────────────────────────────────────────

func TestADM2_HostPrefixWhenSecure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secure *bool
		want   string
	}{
		{"default", nil, csrfCookieHost},
		{"explicit true", boolPtr(true), csrfCookieHost},
		{"explicit false", boolPtr(false), csrfCookie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ensureCSRF(w, httptest.NewRequest(http.MethodGet, "/admin", nil), tc.secure)
			var got string
			for _, c := range w.Result().Cookies() {
				got = c.Name
			}
			if got != tc.want {
				t.Errorf("cookie name = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestADM2_PlantedCookieIsNotAdopted is the bug: ensureCSRF returned any cookie
// of 32 characters or more, so a value planted by a sibling subdomain became the
// token the form echoed back — both halves of the pair in the attacker's hands.
func TestADM2_PlantedCookieIsNotAdopted(t *testing.T) {
	const planted = "planted-value-that-is-long-enough-to-pass"

	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieHost, Value: planted})
	w := httptest.NewRecorder()

	got := ensureCSRF(w, r, nil)
	if got == planted {
		t.Error("a malformed planted cookie was adopted as the CSRF token")
	}
	if !validCSRFToken(got) {
		t.Errorf("minted token %q is not a valid 32-hex token", got)
	}
}

// TestADM2_WellFormedCookieIsReused is the must-still-work half — a real token
// must survive across page loads, or every form would mint a new one.
func TestADM2_WellFormedCookieIsReused(t *testing.T) {
	existing := randomToken()
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.AddCookie(&http.Cookie{Name: csrfCookieHost, Value: existing})

	if got := ensureCSRF(httptest.NewRecorder(), r, nil); got != existing {
		t.Errorf("token = %q, want the existing %q", got, existing)
	}
}

func TestADM2_CheckRejectsMalformedPair(t *testing.T) {
	const planted = "planted-value-that-is-long-enough-to-pass"

	r := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader("_csrf="+planted))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookieHost, Value: planted})
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if checkCSRF(r, nil) {
		t.Error("a matching but malformed cookie/form pair was accepted")
	}
}

func TestADM2_CheckAcceptsGenuinePair(t *testing.T) {
	tok := randomToken()
	r := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader("_csrf="+tok))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookieHost, Value: tok})
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !checkCSRF(r, nil) {
		t.Error("a genuine cookie/form pair was rejected")
	}
}

// ── ADM-3 / ADM-6: what the in-process request carries ───────────────────────

type recordingHandler struct {
	got  *http.Request
	body []byte
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.got = r
	h.body, _ = io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":{}}`))
}

// TestADM3_CallerIdentityIsPropagated is the bug: httptest.NewRequest pins
// RemoteAddr to 192.0.2.1:1234 and Host to example.com, so audit rows named
// nobody and every admin shared one rate-limit bucket.
func TestADM3_CallerIdentityIsPropagated(t *testing.T) {
	rh := &recordingHandler{}
	c := &apiClient{handler: rh, apiPrefix: "/api"}

	src := httptest.NewRequest(http.MethodGet, "/admin/widgets", nil)
	src.RemoteAddr = "203.0.113.9:44321"
	src.Host = "panel.example.org"
	src.TLS = &tls.ConnectionState{}

	if _, _, err := c.do(src, http.MethodGet, "/api/widgets", "", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if rh.got.RemoteAddr != "203.0.113.9:44321" {
		t.Errorf("RemoteAddr = %q, want the caller's", rh.got.RemoteAddr)
	}
	if rh.got.Host != "panel.example.org" {
		t.Errorf("Host = %q, want the caller's", rh.got.Host)
	}
	if rh.got.TLS == nil {
		t.Error("TLS state was not carried over")
	}
}

// TestADM3_RequestContextIsNotPropagated is the deliberate limit: cancelling the
// browser connection must not abort a write the user cannot see fail.
func TestADM3_RequestContextIsNotPropagated(t *testing.T) {
	rh := &recordingHandler{}
	c := &apiClient{handler: rh, apiPrefix: "/api"}

	ctx, cancel := context.WithCancel(context.Background())
	src := httptest.NewRequest(http.MethodGet, "/admin/widgets", nil).WithContext(ctx)
	cancel()

	if _, _, err := c.do(src, http.MethodGet, "/api/widgets", "", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if rh.got.Context().Err() != nil {
		t.Error("the cancelled browser context reached the in-process request")
	}
}

// TestADM6_BodyHeadersAreStrippedFromBodylessGet pins the header-clone fix: a
// submitting POST's multipart Content-Type was cloned onto the GETs issued while
// re-rendering a failed form, describing a body that is not there.
func TestADM6_BodyHeadersAreStrippedFromBodylessGet(t *testing.T) {
	rh := &recordingHandler{}
	c := &apiClient{handler: rh, apiPrefix: "/api"}

	src := httptest.NewRequest(http.MethodPost, "/admin/widgets", nil)
	src.Header.Set("Content-Type", "multipart/form-data; boundary=abc123")
	src.Header.Set("Content-Length", "9999")
	src.Header.Set("Authorization", "Bearer keep-me")

	if _, _, err := c.do(src, http.MethodGet, "/api/widgets", "", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if ct := rh.got.Header.Get("Content-Type"); ct != "" {
		t.Errorf("body-less GET carried Content-Type %q", ct)
	}
	// Identity headers must still come through — that is the whole point of the clone.
	if got := rh.got.Header.Get("Authorization"); got != "Bearer keep-me" {
		t.Errorf("Authorization = %q, want it forwarded", got)
	}
}

// ── ADM-4: response hardening ────────────────────────────────────────────────

func TestADM4_PanelPagesSetSecurityHeaders(t *testing.T) {
	h := admPanel(t, Config{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	for header, want := range map[string]string{
		"Cache-Control":   "no-store",
		"X-Frame-Options": "DENY",
		"Referrer-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestADM4_ErrorPagesSetSecurityHeaders(t *testing.T) {
	h := admPanel(t, Config{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/nonexistent", nil))

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("error page Cache-Control = %q, want no-store", got)
	}
}

// ── ADM-6: nav and static ────────────────────────────────────────────────────

func TestADM6_HeadlessModelIsNotOffered(t *testing.T) {
	h := admPanel(t, Config{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if body := rec.Body.String(); strings.Contains(body, "headless_widgets") {
		t.Error("a Headless model appears in the panel nav; its list view cannot work")
	}
	// The must-still-work half: an ordinary model is still offered.
	if body := rec.Body.String(); !strings.Contains(body, "/admin/widgets") {
		t.Error("the ordinary model vanished from the nav")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/headless_widgets", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("headless list view status = %d, want 404", rec.Code)
	}
}

func TestADM6_StaticDirectoryIsNotListed(t *testing.T) {
	h := admPanel(t, Config{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/static/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("directory listing status = %d, want 404\n%s", rec.Code, rec.Body.String())
	}

	// The must-still-work half: named assets still serve.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/static/admin.css", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("admin.css status = %d, want 200", rec.Code)
	}
}

func boolPtr(b bool) *bool { return &b }
