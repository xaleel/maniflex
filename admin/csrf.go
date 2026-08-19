package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// csrfCookie is the name of the double-submit CSRF cookie.
const csrfCookie = "maniflex_admin_csrf"

// The admin issues browser-originated, state-changing POSTs. It guards them
// with the double-submit-cookie pattern: a random token is stored in a cookie
// and echoed in a hidden form field; a forged cross-site request cannot read
// the cookie to populate the field, so the two will not match.

// ensureCSRF returns the request's CSRF token, minting and setting one if the
// cookie is absent. Call it on every page that renders a form. secure is
// Config.Secure — nil meaning "not set", which is the Secure default.
func ensureCSRF(w http.ResponseWriter, r *http.Request, secureOverride *bool) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	tok := randomToken()
	// Secure unless the operator says otherwise, rather than inferred per request
	// from r.TLS and X-Forwarded-Proto as it was (SEC-9).
	//
	// The header was never verifiable here — a proxy may append rather than
	// replace it, and this package knows nothing about which peers are proxies —
	// but spoofing it was not the problem: the Set-Cookie rides the response to
	// the very request carrying the header, so a forged value only ever
	// downgraded the forger's own cookie.
	//
	// The problem was the direction it failed in. An admin served over plaintext
	// in production issued a non-Secure cookie and said nothing, which is the
	// silent-downgrade shape this audit keeps turning up. Defaulting to Secure
	// inverts it: the panel visibly stops working, which is the truth worth
	// hearing, since an admin panel on plaintext is exposing far more than a CSRF
	// token. http://localhost is a secure context in current Chrome and Firefox,
	// so local development is unaffected; Config.Secure is the way out for a
	// deliberately plaintext panel on a host that is not.
	secure := true
	if secureOverride != nil {
		secure = *secureOverride
	}
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
