package admin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The admin CSRF cookie is Secure by default, whatever the request looks like.
//
// It used to be decided per request from r.TLS or X-Forwarded-Proto (SEC-9).
// That header is not something the framework can verify — a proxy may append
// rather than replace it, and nothing here knows which peers are proxies — but
// the reason for dropping it is not the spoofing, which only ever downgrades the
// spoofer's own cookie. It is that the heuristic failed quietly in the direction
// that matters: an admin panel served over plaintext in production issued a
// non-Secure cookie and nothing anywhere said so. Defaulting to Secure moves
// that failure into the open, where the panel visibly stops working and the
// operator learns they are serving an admin over plaintext (audit S9).
func TestEnsureCSRF_SecureByDefault(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"plain http", func(r *http.Request) { r.TLS = nil }},
		{"direct tls", func(r *http.Request) { r.TLS = &tls.ConnectionState{} }},
		{"proxy says https", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }},
		{"proxy says http", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !csrfSecure(t, nil, tc.mut) {
				t.Errorf("CSRF cookie not marked Secure (%s); the default must not depend on the request", tc.name)
			}
		})
	}
}

// The escape hatch, for a panel deliberately served over plaintext on a host
// browsers do not treat as a secure context.
func TestEnsureCSRF_ExplicitOptOutIsHonoured(t *testing.T) {
	no := false
	if csrfSecure(t, &no, func(*http.Request) {}) {
		t.Error("Config.Secure=false did not turn the Secure attribute off")
	}
}

// An explicit true is the same as the default, and must not be misread as unset.
func TestEnsureCSRF_ExplicitTrueIsHonoured(t *testing.T) {
	yes := true
	if !csrfSecure(t, &yes, func(r *http.Request) { r.TLS = nil }) {
		t.Error("Config.Secure=true was not honoured over plain HTTP")
	}
}

// csrfSecure mints a cookie under the given Config.Secure setting and reports
// whether it carries the Secure attribute.
func csrfSecure(t *testing.T, secure *bool, mut func(*http.Request)) bool {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	mut(r)
	w := httptest.NewRecorder()
	ensureCSRF(w, r, secure)
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookie {
			return c.Secure
		}
	}
	t.Fatalf("no %s cookie was set", csrfCookie)
	return false
}
