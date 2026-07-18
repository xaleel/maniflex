package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type revokableModel struct{ maniflex.BaseModel }

// jsonErrCode pulls the error code out of the response envelope. (The
// equivalent helper in the internal e2e package is not visible from here.)
func jsonErrCode(body []byte) string {
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// revocationServer mounts a model, the JWT middleware wired to rev, and both
// logout actions.
func revocationServer(t *testing.T, secret string, rev auth.Revoker) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{revokableModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{Revoker: rev}))
			srv.Action(auth.Logout(rev, "/logout"))
			srv.Action(auth.LogoutAll(rev, "/logout-all", time.Hour))
		},
	})
}

// bearer builds the Authorization header for a token with the given claims,
// filling in the exp and iat a live issuer would set.
func bearer(t *testing.T, secret, sub, jti string) map[string]string {
	t.Helper()
	now := time.Now()
	return map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, secret, map[string]any{
		"sub": sub,
		"jti": jti,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})}
}

// TestTokenRevocation covers roadmap item 12.1 — logout and the token
// blocklist.
//
//	go test ./tests/e2e/... -run TestTokenRevocation
func TestTokenRevocation(t *testing.T) {
	const secret = "revocation-test-secret-at-least-32-bytes"

	t.Run("logout_revokes_the_calling_token", func(t *testing.T) {
		srv := revocationServer(t, secret, auth.NewMemoryRevoker())
		hdr := bearer(t, secret, "user-1", "jti-1")

		// The token works before logout...
		srv.GET("/revokable_models", hdr).AssertStatus(http.StatusOK)

		srv.POST("/logout", nil, hdr).AssertStatus(http.StatusNoContent)

		// ...and is refused after, though it is still correctly signed and
		// nowhere near its exp. That is the whole point of the feature.
		after := srv.GET("/revokable_models", hdr)
		after.AssertStatus(http.StatusUnauthorized)
		if code := jsonErrCode(after.Body); code != auth.CodeTokenRevoked {
			t.Errorf("error code: got %q want %q", code, auth.CodeTokenRevoked)
		}
	})

	t.Run("logout_does_not_affect_other_sessions", func(t *testing.T) {
		srv := revocationServer(t, secret, auth.NewMemoryRevoker())
		phone := bearer(t, secret, "user-1", "jti-phone")
		laptop := bearer(t, secret, "user-1", "jti-laptop")

		srv.POST("/logout", nil, phone).AssertStatus(http.StatusNoContent)

		srv.GET("/revokable_models", phone).AssertStatus(http.StatusUnauthorized)
		// Same user, different device — logging out here must not log out there.
		srv.GET("/revokable_models", laptop).AssertStatus(http.StatusOK)
	})

	t.Run("logout_all_revokes_every_session_of_the_user", func(t *testing.T) {
		srv := revocationServer(t, secret, auth.NewMemoryRevoker())
		phone := bearer(t, secret, "user-1", "jti-phone")
		laptop := bearer(t, secret, "user-1", "jti-laptop")
		other := bearer(t, secret, "user-2", "jti-other")

		srv.POST("/logout-all", nil, phone).AssertStatus(http.StatusNoContent)

		// Both of this user's sessions die, including the one whose jti the
		// server never saw — that is what a per-token blocklist cannot do.
		srv.GET("/revokable_models", phone).AssertStatus(http.StatusUnauthorized)
		srv.GET("/revokable_models", laptop).AssertStatus(http.StatusUnauthorized)
		// Another user is untouched.
		srv.GET("/revokable_models", other).AssertStatus(http.StatusOK)
	})

	t.Run("token_issued_after_logout_all_still_works", func(t *testing.T) {
		srv := revocationServer(t, secret, auth.NewMemoryRevoker())
		old := bearer(t, secret, "user-1", "jti-old")
		srv.POST("/logout-all", nil, old).AssertStatus(http.StatusNoContent)

		// Logging in again must work — the cutoff kills tokens issued BEFORE
		// it, not the user's account.
		time.Sleep(1100 * time.Millisecond) // iat has one-second resolution
		fresh := bearer(t, secret, "user-1", "jti-fresh")
		srv.GET("/revokable_models", fresh).AssertStatus(http.StatusOK)
	})

	t.Run("token_without_jti_is_refused", func(t *testing.T) {
		srv := revocationServer(t, secret, auth.NewMemoryRevoker())
		now := time.Now()
		hdr := map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, secret, map[string]any{
			"sub": "user-1",
			"iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(),
			// no jti
		})}

		// A token that cannot be revoked would otherwise be permanently exempt
		// from the blocklist — an attacker's preferred token shape.
		resp := srv.GET("/revokable_models", hdr)
		resp.AssertStatus(http.StatusUnauthorized)
		if code := jsonErrCode(resp.Body); code != auth.CodeTokenNotRevocable {
			t.Errorf("error code: got %q want %q", code, auth.CodeTokenNotRevocable)
		}
	})

	t.Run("store_outage_fails_closed_with_503", func(t *testing.T) {
		srv := revocationServer(t, secret, brokenRevoker{})
		hdr := bearer(t, secret, "user-1", "jti-1")

		// The token is perfectly valid; the blocklist simply cannot be reached.
		// Serving it would silently un-revoke every logged-out token for the
		// duration of the outage, so the request is refused instead — and as a
		// 503, because it is the server that is broken, not the credential.
		resp := srv.GET("/revokable_models", hdr)
		resp.AssertStatus(http.StatusServiceUnavailable)
		if code := jsonErrCode(resp.Body); code != auth.CodeRevocationUnavailable {
			t.Errorf("error code: got %q want %q", code, auth.CodeRevocationUnavailable)
		}
	})

	t.Run("no_revoker_configured_changes_nothing", func(t *testing.T) {
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{revokableModel{}},
			Middleware: func(srv *maniflex.Server) {
				srv.Pipeline.Auth.Register(auth.JWTAuth(secret)) // no Revoker
			},
		})
		now := time.Now()
		hdr := map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, secret, map[string]any{
			"sub": "user-1",
			"exp": now.Add(time.Hour).Unix(),
			// deliberately no jti: without a Revoker this must stay legal
		})}
		srv.GET("/revokable_models", hdr).AssertStatus(http.StatusOK)
	})

	t.Run("verify_token_hook_can_refuse", func(t *testing.T) {
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{revokableModel{}},
			Middleware: func(srv *maniflex.Server) {
				srv.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{
					VerifyToken: func(_ *maniflex.ServerContext, claims map[string]any, info *maniflex.AuthInfo) error {
						if claims["tier"] != "paid" {
							return errors.New("subscription required")
						}
						info.Roles = append(info.Roles, "subscriber")
						return nil
					},
				}))
			},
		})
		now := time.Now()
		tokenFor := func(tier string) map[string]string {
			return map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, secret, map[string]any{
				"sub":  "user-1",
				"tier": tier,
				"exp":  now.Add(time.Hour).Unix(),
			})}
		}
		srv.GET("/revokable_models", tokenFor("free")).AssertStatus(http.StatusUnauthorized)
		srv.GET("/revokable_models", tokenFor("paid")).AssertStatus(http.StatusOK)
	})
}

// brokenRevoker stands in for a Redis that is down: every lookup errors.
type brokenRevoker struct{}

var errRevokerDown = errors.New("revocation store unavailable")

func (brokenRevoker) RevokeToken(context.Context, string, time.Time) error { return errRevokerDown }
func (brokenRevoker) IsTokenRevoked(context.Context, string) (bool, error) {
	return false, errRevokerDown
}
func (brokenRevoker) RevokeUser(context.Context, string, time.Time, time.Time) error {
	return errRevokerDown
}
func (brokenRevoker) UserCutoff(context.Context, string) (time.Time, error) {
	return time.Time{}, errRevokerDown
}
