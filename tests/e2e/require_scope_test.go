package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// scopeGuardedServer mounts a model behind JWTAuth + the given scope guard, so
// the assertions below exercise the whole chain: the "scope" claim is parsed
// into AuthInfo.Scopes, and the guard reads it back on the Auth step.
func scopeGuardedServer(t *testing.T, secret string, guard maniflex.MiddlewareFunc) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{ScopesClaim: "scope"}))
			srv.Pipeline.Auth.Register(guard)
		},
	})
}

func scopedToken(t *testing.T, secret, scope string) string {
	t.Helper()
	return makeJWTClaims(t, secret, map[string]any{
		"sub":   "user-1",
		"scope": scope,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
}

// The point of the feature: JWTAuth has always filled AuthInfo.Scopes, and until
// RequireScope nothing read it. This proves the chain closes over a real request.
func TestRequireScope_AllowsATokenCarryingEveryScope(t *testing.T) {
	const secret = "require-scope-secret-at-least-32b"
	s := scopeGuardedServer(t, secret, auth.RequireScope("posts:read", "posts:write"))

	token := scopedToken(t, secret, "posts:read posts:write extra:thing")
	s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token}).
		AssertStatus(http.StatusOK)
}

func TestRequireScope_RefusesATokenMissingAScope(t *testing.T) {
	const secret = "require-scope-secret-at-least-32b"
	s := scopeGuardedServer(t, secret, auth.RequireScope("posts:read", "posts:write"))

	token := scopedToken(t, secret, "posts:read")
	resp := s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token})
	resp.AssertStatus(http.StatusForbidden)
	resp.AssertJSON(func(body map[string]any) {
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("no error envelope in %s", resp.Body)
		}
		if errObj["code"] != "FORBIDDEN" {
			t.Errorf("code: want FORBIDDEN, got %v", errObj["code"])
		}
		msg, _ := errObj["message"].(string)
		if !strings.Contains(msg, "posts:write") {
			t.Errorf("message %q does not name the missing scope", msg)
		}
	})
}

func TestRequireAnyScope_AllowsATokenCarryingOneAcceptedScope(t *testing.T) {
	const secret = "require-any-scope-secret-32-bytes"
	s := scopeGuardedServer(t, secret, auth.RequireAnyScope("posts:write", "admin"))

	token := scopedToken(t, secret, "admin")
	s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token}).
		AssertStatus(http.StatusOK)
}
