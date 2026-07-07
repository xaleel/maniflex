package admin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SEC-9: the admin CSRF cookie must be marked Secure whenever the browser
// connection is TLS — terminated in-process (r.TLS) or at a proxy that forwards
// X-Forwarded-Proto: https — and must NOT be Secure on plain HTTP so local dev
// over http keeps working.
func TestEnsureCSRF_SecureFlag(t *testing.T) {
	csrfSecure := func(t *testing.T, mut func(*http.Request)) bool {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/admin", nil)
		mut(r)
		w := httptest.NewRecorder()
		ensureCSRF(w, r)
		for _, c := range w.Result().Cookies() {
			if c.Name == csrfCookie {
				return c.Secure
			}
		}
		t.Fatalf("no %s cookie was set", csrfCookie)
		return false
	}

	t.Run("plain_http_not_secure", func(t *testing.T) {
		if csrfSecure(t, func(r *http.Request) { r.TLS = nil }) {
			t.Error("CSRF cookie marked Secure over plain HTTP")
		}
	})

	t.Run("direct_tls_secure", func(t *testing.T) {
		if !csrfSecure(t, func(r *http.Request) { r.TLS = &tls.ConnectionState{} }) {
			t.Error("CSRF cookie not marked Secure over direct TLS")
		}
	})

	t.Run("forwarded_proto_https_secure", func(t *testing.T) {
		if !csrfSecure(t, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }) {
			t.Error("CSRF cookie not marked Secure behind an https proxy")
		}
	})

	t.Run("forwarded_proto_http_not_secure", func(t *testing.T) {
		if csrfSecure(t, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }) {
			t.Error("CSRF cookie marked Secure when proxy forwarded plain http")
		}
	})
}
