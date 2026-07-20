package e2e_test

// 11D.9 — a custom token header must tolerate the "Bearer " prefix.
//
// readToken strips "Bearer " only when the header is literally "Authorization".
// Point JWTOptions.Header at anything else and the prefix stays attached, so a
// client sending `X-Auth-Token: Bearer eyJ...` has "Bearer eyJ..." parsed as the
// token — and gets a flat 401 that says the token is invalid rather than that its
// framing is. Sending the prefix is the reflex, because it is what the header it
// replaces requires.
//
// A JWT is dot-separated base64url and can never contain a space, so a leading
// "Bearer " is unambiguously framing and never part of the value.

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type jwtHdrTarget struct{ maniflex.BaseModel }

func TestJWT_CustomHeaderAcceptsBearerPrefix(t *testing.T) {
	const secret = "unit-test-hs256-secret-of-32-bytes!!"

	// Takes t rather than closing over the outer one: testutil.Server keeps the
	// *testing.T it was built with, so a server made from the parent's t reports
	// every subtest's failure against the parent instead of the subtest.
	newSrv := func(t *testing.T, opts ...auth.JWTOptions) *testutil.Server {
		t.Helper()
		return testutil.NewServer(t, testutil.Options{
			Models: []any{jwtHdrTarget{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret, opts...))
			},
		})
	}
	tok := makeJWTClaims(t, secret, map[string]any{
		"sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
	})
	custom := auth.JWTOptions{Header: "X-Auth-Token"}

	t.Run("custom_header_with_prefix", func(t *testing.T) {
		newSrv(t, custom).GET("/jwt_hdr_targets",
			map[string]string{"X-Auth-Token": "Bearer " + tok}).
			AssertStatus(http.StatusOK)
	})

	// The anti-over-reach pair: a bare token on a custom header was the only
	// accepted form and must stay accepted.
	t.Run("custom_header_bare_token", func(t *testing.T) {
		newSrv(t, custom).GET("/jwt_hdr_targets",
			map[string]string{"X-Auth-Token": tok}).
			AssertStatus(http.StatusOK)
	})

	// Authorization keeps requiring the prefix — it is the RFC form, and a bare
	// token there is malformed, not merely unadorned.
	t.Run("authorization_still_requires_prefix", func(t *testing.T) {
		newSrv(t).GET("/jwt_hdr_targets",
			map[string]string{"Authorization": tok}).
			AssertStatus(http.StatusUnauthorized)
	})

	t.Run("authorization_with_prefix_still_works", func(t *testing.T) {
		newSrv(t).GET("/jwt_hdr_targets",
			map[string]string{"Authorization": "Bearer " + tok}).
			AssertStatus(http.StatusOK)
	})

	// RFC 7235 §2.1: the auth scheme name is case-insensitive. Both headers
	// should honour that rather than 401 on a lowercase "bearer".
	t.Run("scheme_is_case_insensitive", func(t *testing.T) {
		newSrv(t).GET("/jwt_hdr_targets",
			map[string]string{"Authorization": "bearer " + tok}).
			AssertStatus(http.StatusOK)
		newSrv(t, custom).GET("/jwt_hdr_targets",
			map[string]string{"X-Auth-Token": "BEARER " + tok}).
			AssertStatus(http.StatusOK)
	})

	// An empty header is still missing, prefix or not — "Bearer " alone carries
	// no token and must not read as one.
	t.Run("prefix_with_no_token_is_rejected", func(t *testing.T) {
		newSrv(t, custom).GET("/jwt_hdr_targets",
			map[string]string{"X-Auth-Token": "Bearer "}).
			AssertStatus(http.StatusUnauthorized)
	})
}
