package e2e_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type jwtExpTarget struct{ maniflex.BaseModel }

// SEC-7: a JWT with no exp claim is valid forever. It is now rejected by default
// (401 TOKEN_MISSING_EXPIRY); JWTOptions.AllowNoExpiry restores acceptance for
// issuers that deliberately mint non-expiring tokens.
func TestJWT_MissingExpiry(t *testing.T) {
	// >= 32 bytes so the weak-secret guard stays silent.
	const secret = "unit-test-hs256-secret-of-32-bytes!!"

	bearer := func(tok string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + tok}
	}
	newSrv := func(opts ...auth.JWTOptions) *testutil.Server {
		return testutil.NewServer(t, testutil.Options{
			Models: []any{jwtExpTarget{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret, opts...))
			},
		})
	}

	noExp := makeJWTClaims(t, secret, map[string]any{"sub": "u1"})

	t.Run("rejected_by_default", func(t *testing.T) {
		resp := newSrv().GET("/jwt_exp_targets", bearer(noExp))
		resp.AssertStatus(http.StatusUnauthorized)
		if code := resp.ErrorCode(); code != "TOKEN_MISSING_EXPIRY" {
			t.Errorf("error code: got %q, want TOKEN_MISSING_EXPIRY", code)
		}
	})

	t.Run("allowed_with_AllowNoExpiry", func(t *testing.T) {
		newSrv(auth.JWTOptions{AllowNoExpiry: true}).
			GET("/jwt_exp_targets", bearer(noExp)).AssertStatus(http.StatusOK)
	})

	t.Run("valid_exp_still_accepted", func(t *testing.T) {
		tok := makeJWTClaims(t, secret, map[string]any{
			"sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
		})
		newSrv().GET("/jwt_exp_targets", bearer(tok)).AssertStatus(http.StatusOK)
	})
}
