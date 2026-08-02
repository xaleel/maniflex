package e2e_test

// AU-2 — auth.AllowAnonymous: a route may tolerate a *missing* credential
// without the authenticator being unregistered there.
//
// The shape under test is the one an app with a hundred endpoints and three
// public ones needs: JWTAuth stays registered globally and strict, and the
// exemption is the thing enumerated. Scoping JWTAuth away from public routes
// instead — the only option before this — inverts the failure mode, because a
// model added later is absent from the inclusion list and so is covered by no
// auth registration at all.
//
// The security core of the feature is the row that must NOT move: a credential
// that was presented and failed is still a 401 on an exempt route. Only absence
// is forgiven. Degrading a bad token to anonymous is how optional auth turns an
// expired session into a silent permission change.

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type AnonPost struct{ maniflex.BaseModel }

// AnonSecret is never named in an AllowAnonymous registration. It stands for
// every model the app did not think about — the one that must stay protected.
type AnonSecret struct{ maniflex.BaseModel }

const anonSecret = "allow-anonymous-hs256-secret-32b!!"

// anonSeen records whether the pipeline reached the DB step, and with what
// principal, so a test can tell "passed through anonymously" from "passed
// through authenticated" rather than reading both as 200.
type anonSeen struct {
	mu       sync.Mutex
	reached  bool
	authed   bool
	userID   string
	tenantID string
}

func (s *anonSeen) middleware() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		s.mu.Lock()
		s.reached = true
		if ctx.Auth != nil {
			s.authed = true
			s.userID = ctx.Auth.UserID
			s.tenantID = ctx.Auth.TenantID
		}
		s.mu.Unlock()
		return next()
	}
}

func (s *anonSeen) get() (reached, authed bool, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reached, s.authed, s.userID
}

// anonServer is the canonical registration: the exemption is listed, the
// authenticator is not scoped. AllowAnonymous is registered first because
// registration order is execution order within a position.
func anonServer(t *testing.T, seen *anonSeen, opts ...auth.JWTOptions) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{AnonPost{}, AnonSecret{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.AllowAnonymous(),
				maniflex.ForModel("AnonPost"),
				maniflex.ForOperation(maniflex.OpList, maniflex.OpRead))
			s.Pipeline.Auth.Register(auth.JWTAuth(anonSecret, opts...))
			if seen != nil {
				s.Pipeline.DB.Register(seen.middleware())
			}
		},
	})
}

func anonToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	return makeJWTClaims(t, anonSecret, claims)
}

// The point of the feature: no Authorization header, and the request is served.
func TestAllowAnonymous_ExemptRouteServesMissingCredential(t *testing.T) {
	t.Parallel()
	var seen anonSeen
	anonServer(t, &seen).GET("/anon_posts").AssertStatus(http.StatusOK)

	reached, authed, _ := seen.get()
	if !reached {
		t.Fatal("the request never reached the DB step")
	}
	if authed {
		t.Error("ctx.Auth must stay nil for an anonymous caller — every " +
			"`ctx.Auth == nil` check in the framework depends on it")
	}
}

// The other half: everything not named in the exemption is unchanged. A model
// nobody thought about is protected by default, which is the whole reason the
// exemption is the enumerated thing rather than the coverage.
func TestAllowAnonymous_IsScopedToItsRegistration(t *testing.T) {
	t.Parallel()

	t.Run("unlisted_model_still_401s", func(t *testing.T) {
		t.Parallel()
		anonServer(t, nil).GET("/anon_secrets").AssertStatus(http.StatusUnauthorized)
	})

	t.Run("unlisted_operation_still_401s", func(t *testing.T) {
		t.Parallel()
		anonServer(t, nil).POST("/anon_posts", map[string]any{}).
			AssertStatus(http.StatusUnauthorized)
	})
}

// "Optional" means verify-if-presented, not skip. A caller who does hold a token
// must arrive authenticated on the exempt route — this is what scoping JWTAuth
// away from the route could never do, because there the middleware never runs
// and even a valid token leaves ctx.Auth nil.
func TestAllowAnonymous_ValidTokenIsStillVerifiedOnAnExemptRoute(t *testing.T) {
	t.Parallel()
	var seen anonSeen
	srv := anonServer(t, &seen)

	tok := anonToken(t, map[string]any{"sub": "u-42"})
	srv.GET("/anon_posts", map[string]string{"Authorization": "Bearer " + tok}).
		AssertStatus(http.StatusOK)

	_, authed, userID := seen.get()
	if !authed {
		t.Fatal("a valid token on an exempt route must still populate ctx.Auth")
	}
	if userID != "u-42" {
		t.Errorf("ctx.Auth.UserID = %q, want %q", userID, "u-42")
	}
}

// The row that must not move. Every one of these presented a credential, so
// every one is a 401 — on the exempt route, with the marker set.
func TestAllowAnonymous_PresentedButFailingCredentialIsStillRefused(t *testing.T) {
	t.Parallel()

	expired := func(t *testing.T) string {
		return anonToken(t, map[string]any{
			"sub": "u1", "exp": time.Now().Add(-time.Hour).Unix(),
		})
	}
	cases := []struct {
		name   string
		header func(*testing.T) string
	}{
		{"expired_token", func(t *testing.T) string { return "Bearer " + expired(t) }},
		{"bad_signature", func(t *testing.T) string {
			return "Bearer " + makeJWTClaims(t, "a-completely-different-secret-32b!", map[string]any{
				"sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
			})
		}},
		{"no_expiry_claim", func(t *testing.T) string {
			return "Bearer " + makeJWTClaims(t, anonSecret, map[string]any{"sub": "u1"})
		}},
		{"missing_bearer_scheme", func(t *testing.T) string {
			return anonToken(t, map[string]any{"sub": "u1"})
		}},
		{"scheme_with_no_token", func(*testing.T) string { return "Bearer " }},
		{"not_a_jwt", func(*testing.T) string { return "Bearer not-a-jwt" }},
		{"wrong_scheme", func(*testing.T) string { return "Basic dXNlcjpwYXNz" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen anonSeen
			resp := anonServer(t, &seen).GET("/anon_posts",
				map[string]string{"Authorization": tc.header(t)})
			if resp.Status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401 — a credential that was presented and "+
					"failed must never degrade to anonymous\n%s", resp.Status, resp.Body)
			}
			if reached, _, _ := seen.get(); reached {
				t.Error("the request reached the DB step after a failed credential")
			}
		})
	}
}

// A custom token header follows the same rule: absent is forgiven, present and
// unparseable is not.
func TestAllowAnonymous_CustomHeader(t *testing.T) {
	t.Parallel()
	custom := auth.JWTOptions{Header: "X-Auth-Token"}

	t.Run("absent_passes", func(t *testing.T) {
		t.Parallel()
		anonServer(t, nil, custom).GET("/anon_posts").AssertStatus(http.StatusOK)
	})

	t.Run("present_and_invalid_401s", func(t *testing.T) {
		t.Parallel()
		anonServer(t, nil, custom).GET("/anon_posts",
			map[string]string{"X-Auth-Token": "garbage"}).
			AssertStatus(http.StatusUnauthorized)
	})

	t.Run("present_and_valid_authenticates", func(t *testing.T) {
		t.Parallel()
		var seen anonSeen
		srv := anonServer(t, &seen, custom)
		srv.GET("/anon_posts", map[string]string{
			"X-Auth-Token": anonToken(t, map[string]any{"sub": "u-9"}),
		}).AssertStatus(http.StatusOK)
		if _, authed, userID := seen.get(); !authed || userID != "u-9" {
			t.Errorf("authed=%v userID=%q, want true/u-9", authed, userID)
		}
	})
}

// Registration order is execution order, so the marker must be set before the
// authenticator reads it. Getting it backwards fails closed — a 401 the
// developer sees on the first request, not an endpoint that quietly opens.
func TestAllowAnonymous_RegisteredAfterTheAuthenticatorFailsClosed(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{AnonPost{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(anonSecret))
			s.Pipeline.Auth.Register(auth.AllowAnonymous()) // too late, on purpose
		},
	})
	srv.GET("/anon_posts").AssertStatus(http.StatusUnauthorized)
}

// JWKSAuth shares jwtMiddleware, so it honours the marker too. The JWKS URL here
// is deliberately unreachable: an anonymous pass must short-circuit before any
// key resolution, so the exempt route answers 200 without a fetch, while a
// request that does present a token fails on the fetch it now has to make.
func TestAllowAnonymous_JWKSAuthHonoursTheMarker(t *testing.T) {
	t.Parallel()
	newSrv := func(t *testing.T) *testutil.Server {
		t.Helper()
		return testutil.NewServer(t, testutil.Options{
			Models: []any{AnonPost{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.AllowAnonymous(),
					maniflex.ForModel("AnonPost"), maniflex.ForOperation(maniflex.OpList))
				s.Pipeline.Auth.Register(auth.JWKSAuth("http://127.0.0.1:1/unreachable.json"))
			},
		})
	}

	t.Run("absent_credential_never_fetches", func(t *testing.T) {
		t.Parallel()
		newSrv(t).GET("/anon_posts").AssertStatus(http.StatusOK)
	})

	t.Run("presented_credential_is_still_verified", func(t *testing.T) {
		t.Parallel()
		newSrv(t).GET("/anon_posts", map[string]string{
			"Authorization": "Bearer " + anonToken(t, map[string]any{"sub": "u1"}),
		}).AssertStatus(http.StatusUnauthorized)
	})
}

// An in-process Execute with no principal stays anonymous-refused unless the
// route says otherwise: AllowAnonymous must not become a second way to satisfy
// trustedPrincipal, whose ctx.Auth != nil half exists precisely so that an
// Execute without a principal is still judged by the app's own auth.
func TestAllowAnonymous_DoesNotWeakenTheExecuteRule(t *testing.T) {
	t.Parallel()
	var seen anonSeen
	srv := anonServer(t, &seen)

	// The unexempted model, reached in-process with no principal: still refused.
	_, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "AnonSecret",
		Operation: maniflex.OpList,
	})
	if err == nil {
		t.Fatal("an in-process call with no principal must still be refused on a " +
			"route that was never exempted")
	}
}
