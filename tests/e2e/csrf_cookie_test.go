package e2e_test

// Audit AUTH-17: the double-submit cookie shipped non-Secure by default and
// adopted whatever value the browser presented as the expected token, so the
// pair proved only that the caller could write a cookie. Two things are pinned
// here: the cookie's attributes, and that a value this server did not mint is
// refused once a Secret is configured.
//
// Why both. A double-submit pair is only as trustworthy as its cookie half, and
// cookies are not origin-scoped: a sibling host under the same registrable
// domain, or an XSS on one, can set csrf_token for the parent and then holds
// both halves — SameSite=Lax is no obstacle, because the forged POST is
// same-site. Secure closes the plaintext route to planting it; the HMAC closes
// the rest, by making a planted value unusable whatever its shape.
//
//	go test ./tests/e2e/... -run 'TestCSRFCookie|TestCSRFSigned'

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const csrfSecret = "csrf-cookie-signing-secret"

// csrfCookieOf returns the named cookie from a set of response headers, or
// fails the test.
func csrfCookieOf(t *testing.T, h http.Header, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie was set; Set-Cookie=%v", name, h.Values("Set-Cookie"))
	return nil
}

// csrfIssued runs a safe request under the given options and returns the cookie
// the middleware issued.
func csrfIssued(t *testing.T, opt auth.CSRFOptions) *http.Cookie {
	t.Helper()
	name := opt.CookieName
	if name == "" {
		name = "csrf_token"
	}
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(opt))
	})
	return csrfCookieOf(t, s.GET("/csrf_models").Header, name)
}

// ── (a) the cookie's attributes ───────────────────────────────────────────────

// Secure is the default, whatever the request looks like. It used to be a bool
// whose zero value was false, so an app that never set the field sent its token
// over plaintext and nothing reported it. A fixed default fails in the open
// instead: the browser withholds the cookie and the app visibly breaks.
func TestCSRFCookie_SecureByDefault(t *testing.T) {
	t.Parallel()
	c := csrfIssued(t, auth.CSRFOptions{})
	if !c.Secure {
		t.Error("the default cookie must be Secure")
	}
	// The three conditions __Host- would additionally require, which this
	// cookie already met and must keep meeting for the opt-in to work.
	if c.Path != "/" {
		t.Errorf("Path: got %q, want /", c.Path)
	}
	if c.Domain != "" {
		t.Errorf("Domain: got %q, want empty — a Domain attribute widens the cookie to siblings", c.Domain)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite: got %v, want Lax", c.SameSite)
	}
}

// The escape hatch, for a service deliberately served over plaintext on a host
// browsers do not treat as a secure context.
func TestCSRFCookie_ExplicitOptOutIsHonoured(t *testing.T) {
	t.Parallel()
	if csrfIssued(t, auth.CSRFOptions{Secure: testutil.Ptr(false)}).Secure {
		t.Error("Secure: &false did not turn the Secure attribute off")
	}
}

// An explicit true is the same as the default and must not be misread as unset:
// the pointer has three states and only two of them may mean Secure.
func TestCSRFCookie_ExplicitTrueIsHonoured(t *testing.T) {
	t.Parallel()
	if !csrfIssued(t, auth.CSRFOptions{Secure: testutil.Ptr(true)}).Secure {
		t.Error("Secure: &true was not honoured")
	}
}

// Naming the cookie __Host-… is the opt-in to the browser-side defence: it
// forces Secure, the one condition of the prefix this middleware did not
// already satisfy. Without that a browser would ignore the prefix entirely and
// the app would be left with a host-wide cookie under a name promising not to
// be one.
func TestCSRFCookie_HostPrefixForcesSecure(t *testing.T) {
	t.Parallel()
	c := csrfIssued(t, auth.CSRFOptions{CookieName: "__Host-csrf_token"})
	if !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Errorf("a __Host- cookie must be Secure, Path=/ and Domain-less: %+v", c)
	}
}

// IssueCSRFCookie takes the same options but runs per login request, so a
// contradiction there cannot be a boot-time panic. It resolves in favour of the
// prefix instead: minting a non-Secure __Host- cookie would hand the browser
// something it drops on the floor, leaving the caller with no token at all.
func TestCSRFCookie_IssueCSRFCookieForcesSecureForAHostPrefix(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	auth.IssueCSRFCookie(w, auth.CSRFOptions{
		CookieName: "__Host-csrf_token",
		Secure:     testutil.Ptr(false),
	})
	if !csrfCookieOf(t, w.Result().Header, "__Host-csrf_token").Secure {
		t.Error("a __Host- cookie must be Secure even against an explicit opt-out")
	}
}

// The contradiction is refused at construction rather than resolved silently
// either way: a non-Secure __Host- cookie is dropped by the browser outright,
// so the app would have no CSRF cookie at all and every write would 403.
func TestCSRFCookie_HostPrefixWithExplicitInsecurePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("__Host- with Secure: &false must panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "__Host-") {
			t.Errorf("the panic should name the prefix: %v", r)
		}
	}()
	auth.CSRF(auth.CSRFOptions{CookieName: "__Host-csrf_token", Secure: testutil.Ptr(false)})
}

// ── (b) a planted cookie value, with a Secret configured ──────────────────────

func signedCSRFServer(t *testing.T) *testutil.Server {
	t.Helper()
	return newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{Secret: csrfSecret}))
	})
}

// The attack this closes: whoever can write the cookie owned the expected
// token, because the middleware read it back as the reference value. Now the
// cookie must carry an HMAC this server minted, so a planted value is refused
// even when the attacker echoes it perfectly in the header.
func TestCSRFSignedCookie_PlantedValueIsRefused(t *testing.T) {
	t.Parallel()
	s := signedCSRFServer(t)

	const planted = "planted-by-a-sibling-host"
	resp := s.POST("/csrf_models", map[string]any{"name": "forged"}, map[string]string{
		"Cookie":       "csrf_token=" + planted,
		"X-CSRF-Token": planted,
	})
	resp.AssertStatus(http.StatusForbidden)
	if got := resp.ErrorCode(); got != "CSRF_COOKIE_INVALID" {
		t.Errorf("error code: got %s, want CSRF_COOKIE_INVALID", got)
	}
	if n := len(s.GET("/csrf_models").DataList()); n != 0 {
		t.Errorf("%d record(s) written despite the 403", n)
	}

	// And a safe request replaces it rather than leaving it in place, so the
	// browser cannot be left holding a token the server will never accept.
	safe := s.GET("/csrf_models", map[string]string{"Cookie": "csrf_token=" + planted})
	if got := csrfCookieOf(t, safe.Header, "csrf_token").Value; got == planted {
		t.Error("a planted cookie must be replaced on the next safe request")
	}
}

// A well-formed "<token>.<mac>" minted with someone else's secret is refused
// too — shape validation alone would have let this through.
func TestCSRFSignedCookie_ForeignSecretIsRefused(t *testing.T) {
	t.Parallel()
	s := signedCSRFServer(t)

	tok := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	mac := hmac.New(sha256.New, []byte("a-completely-different-secret"))
	mac.Write([]byte(tok))
	forged := tok + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	resp := s.POST("/csrf_models", map[string]any{"name": "forged"}, map[string]string{
		"Cookie":       "csrf_token=" + forged,
		"X-CSRF-Token": forged,
	})
	resp.AssertStatus(http.StatusForbidden)
	if got := resp.ErrorCode(); got != "CSRF_COOKIE_INVALID" {
		t.Errorf("error code: got %s, want CSRF_COOKIE_INVALID", got)
	}
}

// Keeping the server's own MAC but swapping the token half it covers must fail:
// the MAC is over the token, so the two cannot be recombined.
func TestCSRFSignedCookie_TamperedTokenHalfIsRefused(t *testing.T) {
	t.Parallel()
	s := signedCSRFServer(t)
	issued := csrfCookieOf(t, s.GET("/csrf_models").Header, "csrf_token").Value

	_, sig, ok := strings.Cut(issued, ".")
	if !ok {
		t.Fatalf("a signed cookie should carry a MAC, got %q", issued)
	}
	tampered := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB." + sig

	s.POST("/csrf_models", map[string]any{"name": "forged"}, map[string]string{
		"Cookie":       "csrf_token=" + tampered,
		"X-CSRF-Token": tampered,
	}).AssertStatus(http.StatusForbidden)
}

// MUST STILL WORK: the honest client's round trip. Read the cookie the server
// issued on a safe request, echo it on an unsafe one.
func TestCSRFSignedCookie_RoundTrips(t *testing.T) {
	t.Parallel()
	s := signedCSRFServer(t)
	tok := csrfCookieOf(t, s.GET("/csrf_models").Header, "csrf_token").Value

	s.POST("/csrf_models", map[string]any{"name": "honest"}, map[string]string{
		"Cookie":       "csrf_token=" + tok,
		"X-CSRF-Token": tok,
	}).AssertStatus(http.StatusCreated)
}

// MUST STILL WORK: the two refusals that were already there keep their codes
// under a Secret, so the new one narrows rather than replaces them.
func TestCSRFSignedCookie_MissingHeaderAndMismatchKeepTheirCodes(t *testing.T) {
	t.Parallel()
	s := signedCSRFServer(t)
	tok := csrfCookieOf(t, s.GET("/csrf_models").Header, "csrf_token").Value

	noHeader := s.POST("/csrf_models", map[string]any{"name": "x"},
		map[string]string{"Cookie": "csrf_token=" + tok})
	noHeader.AssertStatus(http.StatusForbidden)
	if got := noHeader.ErrorCode(); got != "CSRF_TOKEN_MISSING" {
		t.Errorf("missing header: got %s, want CSRF_TOKEN_MISSING", got)
	}

	mismatch := s.POST("/csrf_models", map[string]any{"name": "x"}, map[string]string{
		"Cookie":       "csrf_token=" + tok,
		"X-CSRF-Token": "something-else",
	})
	mismatch.AssertStatus(http.StatusForbidden)
	if got := mismatch.ErrorCode(); got != "CSRF_TOKEN_MISMATCH" {
		t.Errorf("mismatch: got %s, want CSRF_TOKEN_MISMATCH", got)
	}
}

// MUST STILL WORK: signing is opt-in. With no Secret the pair is compared as it
// always was, including a value the app set itself — an SPA that mints its own
// token must not start getting 403s on upgrade.
func TestCSRFCookie_UnsignedPairStillAcceptsAnAppSetValue(t *testing.T) {
	t.Parallel()
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF())
	})
	const own = "a-value-this-app-chose-itself"
	s.POST("/csrf_models", map[string]any{"name": "ok"}, map[string]string{
		"Cookie":       "csrf_token=" + own,
		"X-CSRF-Token": own,
	}).AssertStatus(http.StatusCreated)

	// And it is left in place rather than replaced, since nothing can say it
	// is wrong.
	if sc := s.GET("/csrf_models", map[string]string{"Cookie": "csrf_token=" + own}).
		Header.Values("Set-Cookie"); len(sc) != 0 {
		t.Errorf("an unsigned cookie must not be replaced: %v", sc)
	}
}

// IssueCSRFCookie is the login-rotation path and must mint what the middleware
// will accept — a signed value when a Secret is configured, Secure by default.
func TestCSRFCookie_IssueCSRFCookieMatchesTheMiddleware(t *testing.T) {
	t.Parallel()
	opt := auth.CSRFOptions{Secret: csrfSecret}
	w := httptest.NewRecorder()
	tok := auth.IssueCSRFCookie(w, opt)

	set := csrfCookieOf(t, w.Result().Header, "csrf_token")
	if set.Value != tok {
		t.Errorf("returned token %q is not the cookie value %q", tok, set.Value)
	}
	if !set.Secure {
		t.Error("IssueCSRFCookie must mint a Secure cookie by default")
	}
	if !strings.Contains(tok, ".") {
		t.Errorf("a Secret should produce a signed value, got %q", tok)
	}

	s := signedCSRFServer(t)
	s.POST("/csrf_models", map[string]any{"name": "rotated"}, map[string]string{
		"Cookie":       "csrf_token=" + tok,
		"X-CSRF-Token": tok,
	}).AssertStatus(http.StatusCreated)
}
