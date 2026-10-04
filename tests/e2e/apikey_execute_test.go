package e2e

// Audit AUTH-11: auth.APIKeyAuth steps aside for Server.Execute.
//
// An in-process call has no headers, because it has no client — so an
// authenticator that reads its credential out of the request refused every
// Execute an API-key-protected app made, reads included. The only way through
// was to put the raw key into Invocation.Header on every call, which is the
// secret-in-band pattern the typed principal exists to abolish.
//
// The rule is trustedPrincipal — InProcess *and* a principal — not the bare
// ctx.InProcess() that auth.CSRF uses. CSRF can take the weaker rule because it
// decides nothing about who may call a route; an authenticator cannot, so an
// Execute carrying no principal is still refused here. Both directions are
// asserted below, against the same server, so widening the exemption to
// anonymous in-process calls would fail rather than pass quietly.
//
//	go test ./tests/e2e/... -run TestAPIKeyExecute

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type akxDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

const akxKey = "apikey-execute-service-key"

func akxServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{akxDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
				auth.APIKeyEntry{Key: akxKey, Auth: maniflex.AuthInfo{
					UserID: "svc-1", Roles: []string{"admin"}}}))
		},
	})
}

func akxPrincipal() *maniflex.AuthInfo {
	return &maniflex.AuthInfo{UserID: "svc-1", Roles: []string{"admin"}}
}

// Every operation Execute can raise passes with a principal and no key. Reads
// are listed as well as writes: this authenticator refused them too, which is
// what made Execute unusable rather than merely awkward under an API key.
func TestAPIKeyExecute_PassesWithATypedPrincipal(t *testing.T) {
	t.Parallel()
	mfx := akxServer(t).ManiflexServer()

	res, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "written in process"}, Auth: akxPrincipal(),
	})
	if err != nil {
		t.Fatalf("in-process create: %v", err)
	}
	id, _ := res.Data.(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %+v", res.Data)
	}

	for _, tc := range []struct {
		name string
		inv  maniflex.Invocation
	}{
		{"read", maniflex.Invocation{Operation: maniflex.OpRead, ID: id}},
		{"list", maniflex.Invocation{Operation: maniflex.OpList, Query: url.Values{"limit": {"5"}}}},
		{"update", maniflex.Invocation{Operation: maniflex.OpUpdate, ID: id,
			Body: map[string]any{"title": "patched"}}},
		{"delete", maniflex.Invocation{Operation: maniflex.OpDelete, ID: id}},
	} {
		inv := tc.inv
		inv.Model, inv.Auth = "akxDoc", akxPrincipal()
		if _, err := mfx.Execute(t.Context(), inv); err != nil {
			t.Errorf("in-process %s: %v", tc.name, err)
		}
	}
}

// The principal reaches the pipeline rather than merely getting past the gate:
// an exemption that dropped ctx.Auth would leave every downstream Require* check
// looking at an anonymous request.
func TestAPIKeyExecute_PrincipalSurvivesTheExemption(t *testing.T) {
	t.Parallel()
	var seenUser string
	var seenRoles []string
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{akxDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
				auth.APIKeyEntry{Key: akxKey, Auth: maniflex.AuthInfo{UserID: "svc-1"}}))
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Auth != nil {
					seenUser, seenRoles = ctx.Auth.UserID, ctx.Auth.Roles
				}
				return next()
			})
		},
	})

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "x"}, Auth: akxPrincipal(),
	}); err != nil {
		t.Fatalf("in-process create: %v", err)
	}
	if seenUser != "svc-1" {
		t.Errorf("UserID at the DB step: got %q, want svc-1", seenUser)
	}
	if len(seenRoles) != 1 || seenRoles[0] != "admin" {
		t.Errorf("Roles at the DB step: got %v, want [admin]", seenRoles)
	}
}

// MUST STILL WORK, and the reason the rule is trustedPrincipal rather than a
// bare ctx.InProcess(): an Execute with no principal is anonymous, and this is
// an authenticator. Being internal is not being authenticated.
func TestAPIKeyExecute_AnonymousInProcessCallIsStillRefused(t *testing.T) {
	t.Parallel()
	_, err := akxServer(t).ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "nobody sent this"},
	})
	if err == nil {
		t.Fatal("an in-process call with no principal must still be refused")
	}
	var exec *maniflex.ExecuteError
	if !errors.As(err, &exec) {
		t.Fatalf("want *maniflex.ExecuteError, got %T: %v", err, err)
	}
	if exec.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d %s, want 401", exec.StatusCode, exec.Code)
	}
}

// MUST STILL WORK: the HTTP path is the only one a client can reach, and
// ctx.InProcess() is false on all of it. Asserted against the same server that
// lets an Execute through.
func TestAPIKeyExecute_HTTPStillRequiresTheKey(t *testing.T) {
	t.Parallel()
	srv := akxServer(t)

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "in process"}, Auth: akxPrincipal(),
	}); err != nil {
		t.Fatalf("in-process create: %v", err)
	}

	body := map[string]any{"title": "over http"}
	srv.POST("/akx_docs", body).AssertStatus(http.StatusUnauthorized)
	srv.POST("/akx_docs", body, map[string]string{"X-API-Key": "wrong"}).
		AssertStatus(http.StatusUnauthorized)
	srv.GET("/akx_docs").AssertStatus(http.StatusUnauthorized)
	srv.POST("/akx_docs", body, map[string]string{"X-API-Key": akxKey}).
		AssertStatus(http.StatusCreated)
}

// A key presented in Invocation.Header is not consulted when a principal is
// already attached — trustedPrincipal returns before the lookup, exactly as it
// does in jwtMiddleware. Pinned so the precedence is a decision rather than an
// accident: the typed principal wins, and a stale or wrong key alongside it
// neither grants nor denies anything.
func TestAPIKeyExecute_PrincipalTakesPrecedenceOverAHeaderKey(t *testing.T) {
	t.Parallel()
	mfx := akxServer(t).ManiflexServer()

	h := http.Header{}
	h.Set("X-API-Key", "a-wrong-key")
	if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "x"}, Auth: akxPrincipal(), Header: h,
	}); err != nil {
		t.Errorf("a bad key alongside a principal must not refuse the call: %v", err)
	}

	// MUST STILL WORK: with no principal the key is read, so a valid one in
	// Invocation.Header still authenticates an in-process call.
	h2 := http.Header{}
	h2.Set("X-API-Key", akxKey)
	if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "akxDoc", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "y"}, Header: h2,
	}); err != nil {
		t.Errorf("a valid key with no principal must still authenticate: %v", err)
	}
}
