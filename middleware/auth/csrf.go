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

	"maniflex"
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

	// Secret is required for CSRFSignedToken; it keys the HMAC.
	Secret string

	// CookieName is the name of the double-submit cookie. Default: "csrf_token".
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

	// Secure marks the issued cookie Secure. Default: false (set to true
	// behind TLS).
	Secure bool

	// SameSite controls the issued cookie's SameSite attribute.
	// Default: http.SameSiteLaxMode.
	SameSite http.SameSite

	// TokenLength is the number of random bytes in a double-submit token
	// before base64 encoding. Default: 32.
	TokenLength int
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
//	    AllowedOrigins: []string{"https://admin.example.com", "*.example.com"},
//	    // EnforceBearer defaults to false: bearer-authenticated requests
//	    // are exempt from CSRF checks (bearer tokens are not ambient
//	    // credentials). Set true only if your bearer flow involves
//	    // browser-managed cookies as well.
//	}))
//
// For CSRFSignedToken mode register it AFTER the JWT middleware so that
// ctx.Auth.SessionID is already populated.
func CSRF(opts ...CSRFOptions) maniflex.MiddlewareFunc {
	var opt CSRFOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.applyDefaults()

	if opt.Mode == CSRFSignedToken && opt.Secret == "" {
		panic("auth.CSRF: Secret is required for CSRFSignedToken mode")
	}

	return func(ctx *maniflex.ServerContext, next func() error) error {
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
}

// IssueCSRFCookie writes a fresh double-submit cookie on the response and
// returns the token value. Useful from login Action handlers that want to
// rotate the token after authentication succeeds.
func IssueCSRFCookie(w http.ResponseWriter, opts CSRFOptions) string {
	opts.applyDefaults()
	tok := randomToken(opts.TokenLength)
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

func hasBearer(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func ensureDoubleSubmitCookie(w http.ResponseWriter, r *http.Request, opt *CSRFOptions) {
	if c, err := r.Cookie(opt.CookieName); err == nil && c.Value != "" {
		return
	}
	setCSRFCookie(w, randomToken(opt.TokenLength), opt)
}

func setCSRFCookie(w http.ResponseWriter, value string, opt *CSRFOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     opt.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: false, // JS must read it to echo in the header
		Secure:   opt.Secure,
		SameSite: opt.SameSite,
	})
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
