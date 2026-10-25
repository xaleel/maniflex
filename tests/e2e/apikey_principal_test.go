package e2e_test

// Audit AUTH-18: APIKeyAuth handed every request for one key the configured
// entry's own Roles, Scopes and Claims. A map lookup copies the AuthInfo struct
// but not what its reference fields point at, so middleware writing to the
// principal wrote into the entry — and the entry is the grant for every later
// caller of that key.
//
// The finding read this as a data race needing spare capacity and two
// concurrent requests. It is deterministic, which is why the tests below are
// sequential: one request compacting ctx.Auth.Roles in place stripped a role
// from the entry permanently, and one writing ctx.Auth.Claims handed its value
// to whoever asked next. Sequential tests also fail on every machine rather
// than only under -race.
//
//	go test ./tests/e2e/... -run 'TestAPIKeyPrincipal|TestJWTPrincipal'

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type keyedDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

const keyedSecret = "svc-key-abc"

// keyedServer mounts APIKeyAuth over one entry and runs inspect on the
// authenticated principal, after the authenticator and before anything else.
func keyedServer(t *testing.T, entry maniflex.AuthInfo,
	inspect func(*maniflex.ServerContext)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{keyedDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
				auth.APIKeyEntry{Key: keyedSecret, Auth: entry}))
			if inspect != nil {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					inspect(ctx)
					return next()
				}, maniflex.AtPosition(maniflex.After))
			}
		},
	})
}

func keyedGET(t *testing.T, s *testutil.Server) *testutil.Response {
	t.Helper()
	return s.GET("/keyed_docs", map[string]string{"X-API-Key": keyedSecret})
}

// ── (a) Claims ────────────────────────────────────────────────────────────────

// AuthInfo documents Claims as non-nil whenever Auth is set. It was nil for
// every API-key caller whose entry did not set one, so a middleware enriching
// the principal — the documented reason the field is writable — answered
// 500 PANIC "assignment to entry in nil map", and only for API-key callers, so
// every JWT test passed.
func TestAPIKeyPrincipal_ClaimsIsNonNilAndWritable(t *testing.T) {
	t.Parallel()
	var wasNil bool
	s := keyedServer(t, maniflex.AuthInfo{UserID: "svc-1"},
		func(ctx *maniflex.ServerContext) {
			wasNil = ctx.Auth.Claims == nil
			ctx.Auth.Claims["enriched"] = true
		})
	keyedGET(t, s).AssertStatus(http.StatusOK)
	if wasNil {
		t.Error("Claims was nil; AuthInfo documents it as non-nil whenever Auth is set")
	}
}

// The cross-request leak, which needed no concurrency: request 2 read the value
// request 1 wrote, because both were handed the entry's own map.
func TestAPIKeyPrincipal_ClaimsDoNotBleedBetweenRequests(t *testing.T) {
	t.Parallel()
	var seen []string
	s := keyedServer(t, maniflex.AuthInfo{
		UserID: "svc-1",
		Claims: map[string]any{"plan": "free"},
	}, func(ctx *maniflex.ServerContext) {
		seen = append(seen, fmt.Sprint(ctx.Auth.Claims["caller"]))
		ctx.Auth.Claims["caller"] = ctx.Request.Header.Get("X-Who")
	})

	for _, who := range []string{"alice", "bob", "carol"} {
		s.GET("/keyed_docs", map[string]string{"X-API-Key": keyedSecret, "X-Who": who}).
			AssertStatus(http.StatusOK)
	}
	for i, got := range seen {
		if got != "<nil>" {
			t.Errorf("request %d started with caller=%q, written by an earlier request", i+1, got)
		}
	}

	// MUST STILL WORK: the template's own claims are granted to each request.
	var plan any
	s2 := keyedServer(t, maniflex.AuthInfo{
		UserID: "svc-1",
		Claims: map[string]any{"plan": "free"},
	}, func(ctx *maniflex.ServerContext) { plan = ctx.Auth.Claims["plan"] })
	keyedGET(t, s2).AssertStatus(http.StatusOK)
	if plan != "free" {
		t.Errorf("plan claim: got %v, want free — cloning must not drop the template's claims", plan)
	}
}

// ── (b) Roles and Scopes ──────────────────────────────────────────────────────

// A middleware dropping a role this request may not use compacted the slice in
// place. The entry held the same backing array, so the role was gone for every
// later caller of that key: request 1 was admin and nobody after it was.
func TestAPIKeyPrincipal_RolesAreNotSharedWithTheEntry(t *testing.T) {
	t.Parallel()
	var seen [][]string
	s := keyedServer(t, maniflex.AuthInfo{
		UserID: "svc-1",
		Roles:  []string{"reader", "writer", "admin"},
	}, func(ctx *maniflex.ServerContext) {
		seen = append(seen, slices.Clone(ctx.Auth.Roles))
		ctx.Auth.Roles = slices.DeleteFunc(ctx.Auth.Roles,
			func(r string) bool { return r == "admin" })
	})

	for range 3 {
		keyedGET(t, s).AssertStatus(http.StatusOK)
	}
	want := []string{"reader", "writer", "admin"}
	for i, got := range seen {
		if !slices.Equal(got, want) {
			t.Errorf("request %d was granted %v, want %v — an earlier request edited the entry",
				i+1, got, want)
		}
	}
}

// Same mechanism, same fix, on the OAuth2 scope list.
func TestAPIKeyPrincipal_ScopesAreNotSharedWithTheEntry(t *testing.T) {
	t.Parallel()
	var seen [][]string
	s := keyedServer(t, maniflex.AuthInfo{
		UserID: "svc-1",
		Scopes: []string{"docs:read", "docs:write"},
	}, func(ctx *maniflex.ServerContext) {
		seen = append(seen, slices.Clone(ctx.Auth.Scopes))
		ctx.Auth.Scopes = slices.DeleteFunc(ctx.Auth.Scopes,
			func(s string) bool { return s == "docs:write" })
	})

	for range 2 {
		keyedGET(t, s).AssertStatus(http.StatusOK)
	}
	want := []string{"docs:read", "docs:write"}
	for i, got := range seen {
		if !slices.Equal(got, want) {
			t.Errorf("request %d was granted %v, want %v", i+1, got, want)
		}
	}
}

// The template is copied at registration too, so the slice the caller passed in
// is no longer what the key grants. Otherwise an app that built its role list
// once and reused the variable could change a live grant by accident.
func TestAPIKeyPrincipal_MutatingTheRegisteredSliceDoesNotChangeTheGrant(t *testing.T) {
	t.Parallel()
	roles := []string{"reader"}
	var granted []string
	s := keyedServer(t, maniflex.AuthInfo{UserID: "svc-1", Roles: roles},
		func(ctx *maniflex.ServerContext) { granted = slices.Clone(ctx.Auth.Roles) })

	roles[0] = "admin" // after registration, before the request
	keyedGET(t, s).AssertStatus(http.StatusOK)

	if !slices.Equal(granted, []string{"reader"}) {
		t.Errorf("granted %v; editing the registered slice must not change the grant", granted)
	}
}

// ── (c) IdentityType ──────────────────────────────────────────────────────────

// The field's zero value is documented as "not determined; treat it as a human",
// which cannot apply to the one authenticator whose caller provably is not one.
func TestAPIKeyPrincipal_IdentityTypeIsServiceAccount(t *testing.T) {
	t.Parallel()
	var got maniflex.AuthIdentityType
	s := keyedServer(t, maniflex.AuthInfo{UserID: "svc-1"},
		func(ctx *maniflex.ServerContext) { got = ctx.Auth.IdentityType })
	keyedGET(t, s).AssertStatus(http.StatusOK)
	if got != maniflex.IdentityServiceAccount {
		t.Errorf("IdentityType: got %q, want %q", got, maniflex.IdentityServiceAccount)
	}
}

// MUST STILL WORK: the default is a default. A key issued as somebody's personal
// access token is labelled on the entry and keeps that label.
func TestAPIKeyPrincipal_ExplicitIdentityTypeWins(t *testing.T) {
	t.Parallel()
	var got maniflex.AuthIdentityType
	s := keyedServer(t, maniflex.AuthInfo{UserID: "u-1", IdentityType: maniflex.IdentityHuman},
		func(ctx *maniflex.ServerContext) { got = ctx.Auth.IdentityType })
	keyedGET(t, s).AssertStatus(http.StatusOK)
	if got != maniflex.IdentityHuman {
		t.Errorf("IdentityType: got %q, want %q", got, maniflex.IdentityHuman)
	}
}

// ── (d) the grants themselves ─────────────────────────────────────────────────

// MUST STILL WORK: cloning must not quietly drop what the key grants. Both
// authorizers read the cloned slices, and a role the entry does not hold is
// still refused.
func TestAPIKeyPrincipal_RolesAndScopesAreStillEnforced(t *testing.T) {
	t.Parallel()
	srv := func(guard maniflex.MiddlewareFunc) *testutil.Server {
		return testutil.NewServer(t, testutil.Options{
			Models: []any{keyedDoc{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
					auth.APIKeyEntry{Key: keyedSecret, Auth: maniflex.AuthInfo{
						UserID: "svc-1",
						Roles:  []string{"reader"},
						Scopes: []string{"docs:read"},
					}}))
				s.Pipeline.Auth.Register(guard, maniflex.AtPosition(maniflex.After))
			},
		})
	}

	keyedGET(t, srv(auth.RequireRole("reader"))).AssertStatus(http.StatusOK)
	keyedGET(t, srv(auth.RequireRole("admin"))).AssertStatus(http.StatusForbidden)
	keyedGET(t, srv(auth.RequireScope("docs:read"))).AssertStatus(http.StatusOK)
	keyedGET(t, srv(auth.RequireScope("docs:write"))).AssertStatus(http.StatusForbidden)
}

// MUST STILL WORK: AuthMethod, which was already set per request from the struct
// copy, and whose explicit value already won.
func TestAPIKeyPrincipal_AuthMethodIsUnchanged(t *testing.T) {
	t.Parallel()
	var dflt, explicit string
	s := keyedServer(t, maniflex.AuthInfo{UserID: "svc-1"},
		func(ctx *maniflex.ServerContext) { dflt = ctx.Auth.AuthMethod })
	keyedGET(t, s).AssertStatus(http.StatusOK)

	s2 := keyedServer(t, maniflex.AuthInfo{UserID: "svc-1", AuthMethod: "mtls"},
		func(ctx *maniflex.ServerContext) { explicit = ctx.Auth.AuthMethod })
	keyedGET(t, s2).AssertStatus(http.StatusOK)

	if dflt != "api_key" || explicit != "mtls" {
		t.Errorf("AuthMethod: default %q (want api_key), explicit %q (want mtls)", dflt, explicit)
	}
}

// ── (e) the same invariant on the JWT path ────────────────────────────────────

// Found while fixing the above: a token whose payload is literally `null`
// unmarshals to a nil map and no error, so Claims was nil there too. It takes
// both AllowNoSubject and AllowNoExpiry to get past the registered-claim
// checks, which is why no test had reached it.
func TestJWTPrincipal_NullPayloadStillHasAClaimMap(t *testing.T) {
	t.Parallel()
	const secret = "a-jwt-secret-of-at-least-32-bytes!"
	var wasNil bool
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{keyedDoc{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{
				AllowNoSubject: true,
				AllowNoExpiry:  true,
			}))
			srv.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				wasNil = ctx.Auth.Claims == nil
				ctx.Auth.Claims["enriched"] = true // panicked on a nil map
				return next()
			}, maniflex.AtPosition(maniflex.After))
		},
	})

	s.GET("/keyed_docs", map[string]string{
		"Authorization": "Bearer " + signHMAC(t, "HS256", secret, nil),
	}).AssertStatus(http.StatusOK)
	if wasNil {
		t.Error("a null token payload left Claims nil")
	}
}
