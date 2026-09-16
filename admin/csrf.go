package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// The CSRF cookie has two names, and which one is used follows Config.Secure.
//
// __Host- is the one that matters. The prefix is only honoured by a browser
// when the cookie is Secure, Path=/ and carries no Domain — all three already
// true here — and in exchange the browser refuses to accept that cookie from
// anywhere but the exact origin serving it. That closes the hole this pattern
// otherwise has: cookies are not origin-scoped, so a sibling subdomain (or an
// XSS on one) could set maniflex_admin_csrf for the parent domain, and since
// the attacker then knows both halves of a double-submit pair, SameSite=Lax is
// no obstacle — the forged POST is same-site (audit ADM-2).
//
// The unprefixed name remains for the deliberate Config.Secure=false panel,
// where __Host- would simply be ignored by the browser and leave the panel with
// no CSRF cookie at all.
const (
	csrfCookie     = "maniflex_admin_csrf"
	csrfCookieHost = "__Host-" + csrfCookie
)

// csrfTokenLen is the hex length of a minted token: 16 random bytes.
const csrfTokenLen = 32

// The admin issues browser-originated, state-changing POSTs. It guards them
// with the double-submit-cookie pattern: a random token is stored in a cookie
// and echoed in a hidden form field; a forged cross-site request cannot read
// the cookie to populate the field, so the two will not match.

// csrfSecureFlag resolves Config.Secure. Nil means "not set", which is Secure.
func csrfSecureFlag(secureOverride *bool) bool {
	if secureOverride != nil {
		return *secureOverride
	}
	return true
}

// csrfCookieName returns the cookie name in force for this panel's settings.
func csrfCookieName(secureOverride *bool) string {
	if csrfSecureFlag(secureOverride) {
		return csrfCookieHost
	}
	return csrfCookie
}

// validCSRFToken reports whether a cookie value has the exact shape randomToken
// mints. The old check accepted any value of 32 characters or more, so a cookie
// planted with arbitrary contents was adopted and then echoed back as the
// expected form value.
func validCSRFToken(v string) bool {
	if len(v) != csrfTokenLen {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// ensureCSRF returns the request's CSRF token, minting and setting one if the
// cookie is absent or malformed. Call it on every page that renders a form.
// secureOverride is Config.Secure — nil meaning "not set", which is Secure.
func ensureCSRF(w http.ResponseWriter, r *http.Request, secureOverride *bool) string {
	name := csrfCookieName(secureOverride)
	if c, err := r.Cookie(name); err == nil && validCSRFToken(c.Value) {
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
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   csrfSecureFlag(secureOverride),
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// checkCSRF reports whether a state-changing request carries a form token that
// matches its cookie. The request's form must already be parsed.
func checkCSRF(r *http.Request, secureOverride *bool) bool {
	c, err := r.Cookie(csrfCookieName(secureOverride))
	if err != nil || !validCSRFToken(c.Value) {
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
