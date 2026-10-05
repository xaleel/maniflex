package e2e_test

// Audit AUTH-12: auth.FirstOf accepts any one of several credentials.
//
// The catalogue promised "combine with JWTAuth on the same step to accept
// either credential - the first match wins" and the plain registration gave
// neither half: both authenticators abort when their own header is absent, so
// the pair demanded BOTH credentials, and when both were presented the LAST
// registration's principal won.
//
// The security core is the row that must not move: a credential that was
// presented and failed is refused there and then, and the remaining
// alternatives are NOT tried. Falling through on a bad credential would let a
// caller shed a restrictive identity by corrupting one byte of it, and would
// make a revoked token worth exactly as much as no token.
//
//	go test ./tests/e2e/... -run TestFirstOf

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type fofDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

const (
	fofKey    = "firstof-service-api-key"
	fofSecret = "firstof-hs256-shared-secret-32b!"
)

// fofSeen records the principal the pipeline actually ran with, so "200" can be
// told apart from "200 as the right identity".
type fofSeen struct{ user, method string }

func fofServer(t *testing.T, seen *fofSeen, reg func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{fofDoc{}},
		Middleware: func(s *maniflex.Server) {
			reg(s)
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Auth != nil {
					seen.user, seen.method = ctx.Auth.UserID, ctx.Auth.AuthMethod
				} else {
					seen.user, seen.method = "", "anonymous"
				}
				return next()
			})
		},
	})
}

func fofPair() []maniflex.MiddlewareFunc {
	return []maniflex.MiddlewareFunc{
		auth.APIKeyAuth("X-API-Key", auth.APIKeyEntry{
			Key: fofKey, Auth: maniflex.AuthInfo{UserID: "svc-1"}}),
		auth.JWTAuth(fofSecret),
	}
}

func fofStd(t *testing.T, seen *fofSeen) *testutil.Server {
	t.Helper()
	return fofServer(t, seen, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
	})
}

func fofToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	return makeJWTClaims(t, fofSecret, claims)
}

// The promise: either credential alone is enough, and each lands as its own
// identity rather than as a blank principal.
//
// Run in BOTH registration orders, which is not symmetry for its own sake: a
// member only has to defer a missing credential when a LATER member might carry
// one, so each order exercises a different authenticator's absence branch. With
// the API key listed first, only its branch runs for a token-only caller; with
// the token listed first, only readToken's does. Testing one order leaves the
// other authenticator's deferral unpinned.
func TestFirstOf_EitherCredentialAloneAuthenticates(t *testing.T) {
	t.Parallel()

	keyHdr := map[string]string{"X-API-Key": fofKey}
	for _, order := range []struct {
		name string
		reg  func(*maniflex.Server)
	}{
		{"api key listed first", func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
		}},
		{"token listed first", func(s *maniflex.Server) {
			p := fofPair()
			s.Pipeline.Auth.Register(auth.FirstOf(p[1], p[0]))
		}},
	} {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			tokHdr := map[string]string{
				"Authorization": "Bearer " + fofToken(t, map[string]any{"sub": "u1"}),
			}

			var byKey fofSeen
			fofServer(t, &byKey, order.reg).GET("/fof_docs", keyHdr).
				AssertStatus(http.StatusOK)
			if byKey.user != "svc-1" || byKey.method != "api_key" {
				t.Errorf("api-key-only caller: got %q/%q, want svc-1/api_key",
					byKey.user, byKey.method)
			}

			var byTok fofSeen
			fofServer(t, &byTok, order.reg).GET("/fof_docs", tokHdr).
				AssertStatus(http.StatusOK)
			if byTok.user != "u1" || byTok.method != "jwt" {
				t.Errorf("token-only caller: got %q/%q, want u1/jwt",
					byTok.user, byTok.method)
			}
		})
	}
}

// Order decides precedence only among credentials presented together, and it is
// the FIRST listed member that wins - the semantics the catalogue described and
// the plain registration inverted.
func TestFirstOf_FirstListedMemberWinsWhenBothPresented(t *testing.T) {
	t.Parallel()
	hdr := map[string]string{
		"X-API-Key":     fofKey,
		"Authorization": "Bearer " + fofToken(t, map[string]any{"sub": "u1"}),
	}

	var keyFirst fofSeen
	fofStd(t, &keyFirst).GET("/fof_docs", hdr).AssertStatus(http.StatusOK)
	if keyFirst.method != "api_key" {
		t.Errorf("API key listed first: got %q, want api_key", keyFirst.method)
	}

	var jwtFirst fofSeen
	fofServer(t, &jwtFirst, func(s *maniflex.Server) {
		p := fofPair()
		s.Pipeline.Auth.Register(auth.FirstOf(p[1], p[0]))
	}).GET("/fof_docs", hdr).AssertStatus(http.StatusOK)
	if jwtFirst.method != "jwt" {
		t.Errorf("JWT listed first: got %q, want jwt", jwtFirst.method)
	}
}

// THE SECURITY CORE. A credential that was presented and failed is refused, and
// the alternatives are not tried - so a caller cannot shed a bad token by
// relying on a sibling authenticator to forgive its absence.
func TestFirstOf_APresentedBadCredentialIsRefusedNotDeferred(t *testing.T) {
	t.Parallel()

	expired := makeJWTClaims(t, fofSecret, map[string]any{"sub": "u1", "exp": 1})
	wrongKey := makeJWTClaims(t, "a-completely-different-secret-32b",
		map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"unknown api key", map[string]string{"X-API-Key": "not-a-real-key"}},
		{"expired token", map[string]string{"Authorization": "Bearer " + expired}},
		{"wrongly signed token", map[string]string{"Authorization": "Bearer " + wrongKey}},
		{"token without its scheme", map[string]string{"Authorization": "abc.def.ghi"}},
		// Presented alongside nothing else: if a bad key were treated as absent,
		// JWTAuth would forgive the missing token and FirstOf would answer its own
		// generic 401 instead. Same status, different reason - pinned by message in
		// TestFirstOf_RefusalCarriesTheFailingMembersMessage.
		{"bad key with no token beside it", map[string]string{"X-API-Key": "wrong"}},
		// And the mirror: a bad token with no key beside it.
		{"bad token with no key beside it", map[string]string{"Authorization": "Bearer " + expired}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen fofSeen
			fofStd(t, &seen).GET("/fof_docs", tc.hdr).AssertStatus(http.StatusUnauthorized)
			if seen.method != "" {
				t.Errorf("the request reached the DB step as %q/%q; a refused "+
					"credential must not run the pipeline", seen.user, seen.method)
			}
		})
	}
}

// The sharpest shape, and the one a status-only assertion misses: a caller
// presents a BAD token alongside a VALID API key, with the token listed first.
//
// Deferring the bad token would hand them the API key's identity instead of a
// refusal - the "shed a restrictive role by corrupting one byte of your own
// token" move AllowAnonymous's own doc warns about, except the fallback here is
// another identity rather than anonymity. The plain bad-credential rows above
// cannot catch this: deferral still ends in 401 when nothing else matches, so
// the status is unchanged and only the reason differs.
func TestFirstOf_ABadCredentialIsNotDowngradedToASiblingIdentity(t *testing.T) {
	t.Parallel()
	expired := makeJWTClaims(t, fofSecret, map[string]any{"sub": "u1", "exp": 1})

	var seen fofSeen
	fofServer(t, &seen, func(s *maniflex.Server) {
		p := fofPair()
		s.Pipeline.Auth.Register(auth.FirstOf(p[1], p[0])) // token examined first
	}).GET("/fof_docs", map[string]string{
		"Authorization": "Bearer " + expired,
		"X-API-Key":     fofKey,
	}).AssertStatus(http.StatusUnauthorized)

	if seen.method != "" {
		t.Errorf("a bad token was downgraded to the %q identity instead of being "+
			"refused; a presented credential that fails must stop the chain", seen.method)
	}
}

// A bad credential keeps its OWN authenticator's message rather than being
// flattened into the generic "none presented" refusal, which is what makes the
// failure debuggable - and is the observable difference between "refused" and
// "deferred then refused".
func TestFirstOf_RefusalCarriesTheFailingMembersMessage(t *testing.T) {
	t.Parallel()
	var seen fofSeen
	srv := fofStd(t, &seen)

	bad := srv.GET("/fof_docs", map[string]string{"X-API-Key": "wrong"}).
		AssertStatus(http.StatusUnauthorized)
	if !strings.Contains(string(bad.Body), "invalid API key") {
		t.Errorf("want the API key authenticator's own message, got %s", bad.Body)
	}

	none := srv.GET("/fof_docs").AssertStatus(http.StatusUnauthorized)
	if !strings.Contains(string(none.Body), "no recognised credential") {
		t.Errorf("want the FirstOf message when nothing was presented, got %s", none.Body)
	}
}

// No credential at all is 401 - FirstOf answers it, since no member does.
func TestFirstOf_NoCredentialIsRefused(t *testing.T) {
	t.Parallel()
	var seen fofSeen
	fofStd(t, &seen).GET("/fof_docs").AssertStatus(http.StatusUnauthorized)
	if seen.method != "" {
		t.Errorf("an unauthenticated request reached the DB step as %q", seen.method)
	}
}

// AllowAnonymous still means what it means: with no credential the route is
// served and ctx.Auth stays nil, so every ctx.Auth == nil check in the
// framework keeps working. A presented-and-failed credential is still 401.
func TestFirstOf_HonoursAllowAnonymous(t *testing.T) {
	t.Parallel()
	newSrv := func(t *testing.T, seen *fofSeen) *testutil.Server {
		return fofServer(t, seen, func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.AllowAnonymous(),
				maniflex.ForModel("fofDoc"), maniflex.ForOperation(maniflex.OpList))
			s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
		})
	}

	var anon fofSeen
	newSrv(t, &anon).GET("/fof_docs").AssertStatus(http.StatusOK)
	if anon.method != "anonymous" {
		t.Errorf("ctx.Auth must stay nil for an anonymous caller, got %q", anon.method)
	}

	// MUST STILL WORK: only absence is forgiven, never a bad credential.
	var bad fofSeen
	newSrv(t, &bad).GET("/fof_docs", map[string]string{"X-API-Key": "wrong"}).
		AssertStatus(http.StatusUnauthorized)

	// And a real credential still lands as a principal rather than as anonymous.
	var good fofSeen
	newSrv(t, &good).GET("/fof_docs", map[string]string{"X-API-Key": fofKey}).
		AssertStatus(http.StatusOK)
	if good.user != "svc-1" {
		t.Errorf("a presented credential must still authenticate, got %q", good.user)
	}
}

// MUST STILL WORK: Server.Execute passes with a typed principal, as each member
// does on its own (AUTH-11), and is still refused when it carries none.
func TestFirstOf_ExecuteRule(t *testing.T) {
	t.Parallel()
	var seen fofSeen
	mfx := fofStd(t, &seen).ManiflexServer()

	if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "fofDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "in process"},
		Auth: &maniflex.AuthInfo{UserID: "svc-1"},
	}); err != nil {
		t.Fatalf("in-process create with a principal: %v", err)
	}

	if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "fofDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "nobody"},
	}); err == nil {
		t.Error("an in-process call with no principal must still be refused")
	}
}

// The tolerance marker must not leak past FirstOf's own members: an
// authenticator registered after it still refuses a missing credential.
func TestFirstOf_ToleranceDoesNotLeakToLaterMiddleware(t *testing.T) {
	t.Parallel()
	var seen fofSeen
	srv := fofServer(t, &seen, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
		// A second, independent authenticator on a different header. It is not
		// part of the FirstOf, so its credential stays mandatory.
		s.Pipeline.Auth.Register(auth.APIKeyAuth("X-Second-Key", auth.APIKeyEntry{
			Key: "second", Auth: maniflex.AuthInfo{UserID: "svc-2"}}))
	})

	srv.GET("/fof_docs", map[string]string{"X-API-Key": fofKey}).
		AssertStatus(http.StatusUnauthorized)
	srv.GET("/fof_docs", map[string]string{
		"X-API-Key": fofKey, "X-Second-Key": "second"}).AssertStatus(http.StatusOK)
}

// FirstOf vouches only for an identity one of ITS OWN members established.
//
// An app that installs a principal in an earlier Auth middleware and then
// registers FirstOf gets a 401 when none of the alternatives matches, rather
// than FirstOf waving the request through on the strength of someone else's
// work. That is the same judgement jwtMiddleware records for itself - a
// short-circuit on a non-nil ctx.Auth "would be a real weakening", because an
// app whose earlier middleware trusts a header would silently start keeping it.
// An authenticator of your own belongs in the FirstOf list, where it is one of
// the alternatives rather than a bypass around them.
func TestFirstOf_DoesNotVouchForAnEarlierMiddlewaresPrincipal(t *testing.T) {
	t.Parallel()
	var seen fofSeen
	srv := fofServer(t, &seen, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Auth = &maniflex.AuthInfo{UserID: "set-by-an-earlier-middleware"}
			return next()
		})
		s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
	})

	// No credential for either member: FirstOf refuses, despite ctx.Auth being set.
	srv.GET("/fof_docs").AssertStatus(http.StatusUnauthorized)

	// MUST STILL WORK: a member that does match wins, and replaces that principal.
	var matched fofSeen
	fofServer(t, &matched, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Auth = &maniflex.AuthInfo{UserID: "set-by-an-earlier-middleware"}
			return next()
		})
		s.Pipeline.Auth.Register(auth.FirstOf(fofPair()...))
	}).GET("/fof_docs", map[string]string{"X-API-Key": fofKey}).
		AssertStatus(http.StatusOK)
	if matched.user != "svc-1" {
		t.Errorf("a matching member must install its own principal, got %q", matched.user)
	}
}

func TestFirstOf_PanicsOnFewerThanTwoMembers(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("FirstOf with %d members must panic", n)
				}
			}()
			auth.FirstOf(fofPair()[:n]...)
		}()
	}
}
