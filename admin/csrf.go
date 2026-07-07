package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// csrfCookie is the name of the double-submit CSRF cookie.
const csrfCookie = "maniflex_admin_csrf"

// The admin issues browser-originated, state-changing POSTs. It guards them
// with the double-submit-cookie pattern: a random token is stored in a cookie
// and echoed in a hidden form field; a forged cross-site request cannot read
// the cookie to populate the field, so the two will not match.

// ensureCSRF returns the request's CSRF token, minting and setting one if the
// cookie is absent. Call it on every page that renders a form.
func ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	tok := randomToken()
	// Mark the cookie Secure when the browser connection is TLS — either
	// terminated in-process (r.TLS) or at a proxy that forwards
	// X-Forwarded-Proto: https — so it can't ride a plaintext HTTP request (SEC-9).
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// checkCSRF reports whether a state-changing request carries a form token that
// matches its cookie. The request's form must already be parsed.
func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	got := r.FormValue("_csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

// randomToken returns a 32-byte hex-encoded random string.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
