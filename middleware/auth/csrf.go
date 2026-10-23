package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/internal/accessdecision"
)

// CSRFMode selects between the two supported CSRF defence strategies.
type CSRFMode int

const (
	// CSRFDoubleSubmit issues a random token in a non-HttpOnly cookie and
	// requires the client to echo it in a request header on unsafe methods.
	CSRFDoubleSubmit CSRFMode = iota

	// CSRFSignedToken derives the expected token as HMAC(SessionID, Secret).
	// Validation is stateless — no cookie is issued by this middleware.
	// Requires ctx.Auth.SessionID to be populated by an earlier auth step
	// (e.g. JWT "jti" claim).
	CSRFSignedToken
)

// CSRFOptions configures CSRF behaviour. The zero value is valid and uses
// CSRFDoubleSubmit with sensible defaults.
type CSRFOptions struct {
	// Mode selects the validation strategy. Default: CSRFDoubleSubmit.
	Mode CSRFMode

	// Secret keys the HMAC. It is required for CSRFSignedToken.
	//
	// In CSRFDoubleSubmit mode it is optional, and setting one changes what
	// the cookie holds: the minted value carries an HMAC of itself, so a
	// cookie this server did not mint is refused rather than adopted as the
	// expected token. That is what closes cookie tossing. A double-submit
	// pair is only as trustworthy as its cookie half, and cookies are not
	// origin-scoped: a sibling host under the same registrable domain — or an
	// XSS on one — can set csrf_token for the parent and then holds both
	// halves. Without a Secret the two halves are still compared, exactly as
	// before; set one for any app whose domain has hosts it does not control
	// (audit AUTH-17).
	Secret string

	// CookieName is the name of the double-submit cookie. Default: "csrf_token".
	//
	// A name carrying the "__Host-" prefix is honoured: it forces Secure, the
	// one condition this middleware did not already meet. A browser respects
	// the prefix only on a Secure, Path=/, Domain-less cookie, and in return
	// refuses to accept that cookie from any origin but the exact one serving
	// it — the other half of the cookie-tossing defence, and the one the
	// admin panel uses. Opt-in rather than automatic, because this cookie is
	// non-HttpOnly precisely so your client can read it, and renaming it is
	// the client's business (audit AUTH-17).
	CookieName string

	// HeaderName is the request header clients echo the token in.
	// Default: "X-CSRF-Token".
	HeaderName string

	// AllowedOrigins is an optional Origin / Referer allowlist applied to
	// unsafe methods. Entries may be exact ("https://example.com") or a
	// host pattern with a leading-label wildcard ("*.example.com"). When
	// the slice is empty no Origin/Referer check is performed.
	AllowedOrigins []string

	// EnforceBearer makes the middleware run CSRF checks even on requests
	// that carry an `Authorization: Bearer` header. The zero value (false,
	// the default) skips enforcement for bearer-authenticated requests —
	// bearer tokens from JS are not ambient credentials, so they are not
	// CSRF-vulnerable. Set this to true only if your bearer flow involves
	// browser-managed cookies as well.
	//
	// The field was previously named SkipBearer and defaulted to true,
	// which silently flipped to false whenever a caller passed any other
	// options field — see roadmap §11B.2. Renaming + inverting lets the
	// zero value be the safe default.
	EnforceBearer bool

	// Secure marks the issued cookie Secure. Nil, the default, means Secure.
	//
	// It was a bool, so the zero value was the insecure one: an app that
	// never set it sent its CSRF token over plaintext HTTP and nothing
	// anywhere said so. A fixed secure default fails in the open instead —
	// the browser withholds the cookie and the app visibly stops working,
	// which is the truth worth hearing. http://localhost is a secure context
	// in current browsers, so local development is unaffected; point this at
	// a false for a service deliberately served over plaintext on a host that
	// is not. Mirrors admin.Config.Secure, which took the same shape for the
	// same reason (audit SEC-9, AUTH-17).
	Secure *bool

	// SameSite controls the issued cookie's SameSite attribute.
	// Default: http.SameSiteLaxMode.
	SameSite http.SameSite

	// TokenLength is the number of random bytes in a double-submit token
	// before base64 encoding. Default: 32.
	TokenLength int
}

// hostCookiePrefix is the cookie-name prefix a browser honours only on a
// Secure, Path=/, Domain-less cookie, and whose cookie it then accepts from no
// origin but the exact one that served it.
const hostCookiePrefix = "__Host-"

// secureCookie resolves Secure: nil means Secure, and a __Host- name forces it
// since a browser would otherwise ignore the prefix and leave the app with a
// host-wide cookie under a name that promised not to be one.
func (o *CSRFOptions) secureCookie() bool {
	if strings.HasPrefix(o.CookieName, hostCookiePrefix) {
		return true
	}
	if o.Secure != nil {
		return *o.Secure
	}
	return true
}

func (o *CSRFOptions) applyDefaults() {
	if o.CookieName == "" {
		o.CookieName = "csrf_token"
	}
	if o.HeaderName == "" {
		o.HeaderName = "X-CSRF-Token"
	}
	if o.SameSite == 0 {
		o.SameSite = http.SameSiteLaxMode
	}
	if o.TokenLength <= 0 {
		o.TokenLength = 32
	}
}

// CSRF returns a middleware that protects unsafe HTTP methods against
// cross-site request forgery. Register it on the Auth pipeline step:
//
//	server.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
//	    Mode:           auth.CSRFDoubleSubmit,
//	    Secret:         os.Getenv("CSRF_SECRET"), // signs the cookie; see Secret
//	    AllowedOrigins: []string{"https://admin.example.com", "*.example.com"},
//	    // EnforceBearer defaults to false: bearer-authenticated requests
//	    // are exempt from CSRF checks (bearer tokens are not ambient
//	    // credentials). Set true only if your bearer flow involves
//	    // browser-managed cookies as well.
//	}))
//
// The issued cookie is Secure unless CSRFOptions.Secure says otherwise.
//
// For CSRFSignedToken mode register it AFTER the JWT middleware so that
// ctx.Auth.SessionID is already populated.
//
// A request raised by Server.Execute passes untouched. CSRF defends a browser
// against being tricked into spending credentials it attaches by itself, and an
// in-process call has no browser to trick, no cookie jar to spend from and no
// header a caller could echo one back in — the check can only ever refuse it.
//
// The rule here is ctx.InProcess() alone, deliberately weaker than the
// trustedPrincipal rule JWTAuth and JWKSAuth use (InProcess *and* a principal).
// Their extra condition is what keeps an Execute with no principal anonymous, so
// that the app's own auth refuses it. This middleware is not the app's own auth:
// it is marked as deciding nothing (see MarkNotADecision below), so applying the
// same condition here would answer a principal-less Execute with 403
// CSRF_TOKEN_MISSING — a transport complaint standing in front of the 401 the
// authenticator was about to give, naming the wrong problem. Stepping aside
// lets the authenticator answer. See ServerContext.InProcess, which is derived
// from a field only Execute can set: over HTTP this branch is never taken.
func CSRF(opts ...CSRFOptions) maniflex.MiddlewareFunc {
	var opt CSRFOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.applyDefaults()

	if opt.Mode == CSRFSignedToken && opt.Secret == "" {
		panic("auth.CSRF: Secret is required for CSRFSignedToken mode")
	}
	// Refusing the contradiction rather than silently resolving it either way:
	// a non-Secure __Host- cookie is dropped by the browser outright, so the
	// app would have no CSRF cookie at all and every write would answer 403.
	if strings.HasPrefix(opt.CookieName, hostCookiePrefix) && opt.Secure != nil && !*opt.Secure {
		panic("auth.CSRF: CookieName " + opt.CookieName + " carries the " +
			hostCookiePrefix + " prefix, which a browser honours only on a Secure " +
			"cookie; drop the prefix or leave Secure unset")
	}

	fn := func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.InProcess() {
			return next()
		}

		r := ctx.Request

		if isSafeMethod(r.Method) {
			if opt.Mode == CSRFDoubleSubmit {
				ensureDoubleSubmitCookie(ctx.Writer, r, &opt)
			}
			return next()
		}

		// Unsafe method — enforce.

		if !opt.EnforceBearer && hasBearer(r) {
			return next()
		}

		if len(opt.AllowedOrigins) > 0 && !originAllowed(r, opt.AllowedOrigins) {
			ctx.Abort(http.StatusForbidden, "CSRF_ORIGIN_REJECTED",
				"request Origin/Referer is not in the allowed list")
			return nil
		}

		presented := r.Header.Get(opt.HeaderName)
		if presented == "" {
			ctx.Abort(http.StatusForbidden, "CSRF_TOKEN_MISSING",
				"missing "+opt.HeaderName+" header")
			return nil
		}

		var expected string
		switch opt.Mode {
		case CSRFDoubleSubmit:
			c, err := r.Cookie(opt.CookieName)
			if err != nil || c.Value == "" {
				ctx.Abort(http.StatusForbidden, "CSRF_COOKIE_MISSING",
					"missing "+opt.CookieName+" cookie")
				return nil
			}
			// With a Secret configured the cookie half must prove this server
			// minted it. Without one this is the unsigned pair as before, and
			// whatever the browser presents is the reference value.
			if !doubleSubmitCookieValid(c.Value, &opt) {
				ctx.Abort(http.StatusForbidden, "CSRF_COOKIE_INVALID",
					"the "+opt.CookieName+" cookie was not issued by this server")
				return nil
			}
			expected = c.Value
		case CSRFSignedToken:
			if ctx.Auth == nil || ctx.Auth.SessionID == "" {
				ctx.Abort(http.StatusForbidden, "CSRF_NO_SESSION",
					"signed CSRF requires an authenticated session")
				return nil
			}
			expected = signedToken(ctx.Auth.SessionID, opt.Secret)
		}

		if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			ctx.Abort(http.StatusForbidden, "CSRF_TOKEN_MISMATCH",
				"CSRF token mismatch")
			return nil
		}
		return next()
	}
	// Transport protection, not an access decision: it passes every safe method
	// untouched, and on writes checks only that a cookie matches a header, which
	// any non-browser client sets for itself. Registered without an
	// authenticator it leaves every route open, so it must not satisfy
	// ValidateProduction (audit AUTH-6).
	accessdecision.MarkNotADecision(fn)
	return fn
}

// IssueCSRFCookie writes a fresh double-submit cookie on the response and
// returns the token value. Useful from login Action handlers that want to
// rotate the token after authentication succeeds.
func IssueCSRFCookie(w http.ResponseWriter, opts CSRFOptions) string {
	opts.applyDefaults()
	tok := mintDoubleSubmitToken(&opts)
	setCSRFCookie(w, tok, &opts)
	return tok
}

// SignedCSRFToken returns the HMAC-derived token for the given session ID
// and secret. Useful from a login handler when issuing a JWT so the response
// body can include the matching CSRF token for the SPA to store.
func SignedCSRFToken(sessionID, secret string) string {
	return signedToken(sessionID, secret)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// hasBearer reports whether the request authenticates with a bearer token, in
// which case it is not cookie-borne and CSRF does not apply.
//
// Shares stripBearer with JWTAuth so the two agree on what a bearer header is.
// They must: this decides whether CSRF is enforced and JWTAuth decides whether
// the token is read, so a header one accepted and the other did not would
// authenticate the caller and then demand a CSRF token from them anyway.
func hasBearer(r *http.Request) bool {
	_, ok := stripBearer(r.Header.Get("Authorization"))
	return ok
}

func ensureDoubleSubmitCookie(w http.ResponseWriter, r *http.Request, opt *CSRFOptions) {
	if c, err := r.Cookie(opt.CookieName); err == nil && doubleSubmitCookieValid(c.Value, opt) {
		return
	}
	setCSRFCookie(w, mintDoubleSubmitToken(opt), opt)
}

func setCSRFCookie(w http.ResponseWriter, value string, opt *CSRFOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     opt.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: false, // JS must read it to echo in the header
		Secure:   opt.secureCookie(),
		SameSite: opt.SameSite,
	})
}

// mintDoubleSubmitToken returns a fresh cookie value. With a Secret configured
// it is "<random>.<HMAC of that random>", so the server can later tell its own
// token from one someone else wrote into the cookie jar. The MAC is over the
// encoded text, not the raw bytes, so verification never has to decode.
func mintDoubleSubmitToken(opt *CSRFOptions) string {
	tok := randomToken(opt.TokenLength)
	if opt.Secret == "" {
		return tok
	}
	return tok + "." + doubleSubmitMAC(tok, opt.Secret)
}

// doubleSubmitCookieValid reports whether value may be used as the expected
// token. With no Secret every non-empty value qualifies, which is the unsigned
// behaviour this middleware has always had — an app may set the cookie itself.
func doubleSubmitCookieValid(value string, opt *CSRFOptions) bool {
	if value == "" {
		return false
	}
	if opt.Secret == "" {
		return true
	}
	tok, sig, ok := strings.Cut(value, ".")
	if !ok || tok == "" {
		return false
	}
	want := doubleSubmitMAC(tok, opt.Secret)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

func doubleSubmitMAC(token, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail; if it does, panic — we cannot
		// safely issue a predictable token.
		panic("auth.CSRF: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func signedToken(sessionID, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// originAllowed checks the request Origin first, falling back to Referer.
// Absent both, the request is rejected when an allowlist is configured.
func originAllowed(r *http.Request, allowed []string) bool {
	host, ok := requestOriginHost(r)
	if !ok {
		return false
	}
	for _, pat := range allowed {
		if originMatches(pat, r, host) {
			return true
		}
	}
	return false
}

// requestOriginHost extracts the host (no scheme, no port) from the request's
// Origin or Referer header. Returns ok=false when neither is present.
func requestOriginHost(r *http.Request) (string, bool) {
	for _, raw := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		return u.Hostname(), true
	}
	return "", false
}

// originMatches reports whether pat allows the request. Three forms supported:
//   - "*.example.com"      → matches any subdomain (one or more labels)
//   - "https://example.com" → matches exact origin URL
//   - "example.com"        → matches exact host
func originMatches(pat string, r *http.Request, host string) bool {
	if strings.HasPrefix(pat, "*.") {
		suffix := pat[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	if strings.Contains(pat, "://") {
		// Compare against the full Origin URL when one was sent.
		raw := r.Header.Get("Origin")
		if raw == "" {
			raw = r.Header.Get("Referer")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		return u.Scheme+"://"+u.Host == pat
	}
	return host == pat
}
