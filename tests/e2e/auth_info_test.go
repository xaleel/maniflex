package e2e_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// makeJWTClaims builds an HS256 JWT from an arbitrary claims map.
func makeJWTClaims(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("makeJWTClaims: marshal: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

// captureAuthMiddleware returns a DB-step Before middleware that copies
// ctx.Auth into the provided pointer under the given mutex.
func captureAuthMiddleware(mu *sync.Mutex, out **maniflex.AuthInfo) func(*maniflex.ServerContext, func() error) error {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth != nil {
			cp := *ctx.Auth
			mu.Lock()
			*out = &cp
			mu.Unlock()
		}
		return next()
	}
}

type minimalModel struct{ maniflex.BaseModel }

func TestAuthInfo_JWTPopulatesNewFields(t *testing.T) {
	const secret = "test-secret"

	var mu sync.Mutex
	var captured *maniflex.AuthInfo

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{
				TenantClaim: "tenant_id",
				ScopesClaim: "scope",
			}))
			srv.Pipeline.DB.Register(
				captureAuthMiddleware(&mu, &captured),
				maniflex.AtPosition(maniflex.Before),
			)
		},
	})

	token := makeJWTClaims(t, secret, map[string]any{
		"sub":       "user-42",
		"roles":     []string{"editor"},
		"jti":       "session-xyz",
		"tenant_id": "acme",
		"scope":     "read write",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})

	s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token})

	mu.Lock()
	got := captured
	mu.Unlock()

	if got == nil {
		t.Fatal("ctx.Auth was nil — auth middleware did not run or request failed")
	}
	if got.UserID != "user-42" {
		t.Errorf("UserID: want user-42, got %q", got.UserID)
	}
	if got.TenantID != "acme" {
		t.Errorf("TenantID: want acme, got %q", got.TenantID)
	}
	if got.SessionID != "session-xyz" {
		t.Errorf("SessionID: want session-xyz, got %q", got.SessionID)
	}
	if got.AuthMethod != "jwt" {
		t.Errorf("AuthMethod: want jwt, got %q", got.AuthMethod)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "read" || got.Scopes[1] != "write" {
		t.Errorf("Scopes: want [read write], got %v", got.Scopes)
	}
}

func TestAuthInfo_TenantClaimNotSetWhenOptionEmpty(t *testing.T) {
	const secret = "s"

	var mu sync.Mutex
	var captured *maniflex.AuthInfo

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			// No TenantClaim configured → TenantID must stay empty.
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret))
			srv.Pipeline.DB.Register(
				captureAuthMiddleware(&mu, &captured),
				maniflex.AtPosition(maniflex.Before),
			)
		},
	})

	token := makeJWTClaims(t, secret, map[string]any{
		"sub":       "u1",
		"tenant_id": "should-not-appear",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})

	s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token})

	mu.Lock()
	got := captured
	mu.Unlock()

	if got == nil {
		t.Fatal("ctx.Auth nil")
	}
	if got.TenantID != "" {
		t.Errorf("TenantID: want empty (no TenantClaim set), got %q", got.TenantID)
	}
}

func TestAuthInfo_ScopesArrayClaim(t *testing.T) {
	const secret = "s"

	var mu sync.Mutex
	var captured *maniflex.AuthInfo

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret))
			srv.Pipeline.DB.Register(
				captureAuthMiddleware(&mu, &captured),
				maniflex.AtPosition(maniflex.Before),
			)
		},
	})

	token := makeJWTClaims(t, secret, map[string]any{
		"sub":   "u1",
		"scope": []string{"openid", "profile", "email"},
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	s.GET("/minimal_models", map[string]string{"Authorization": "Bearer " + token})

	mu.Lock()
	got := captured
	mu.Unlock()

	if got == nil {
		t.Fatal("ctx.Auth nil")
	}
	if len(got.Scopes) != 3 {
		t.Errorf("Scopes: want 3 items, got %v", got.Scopes)
	}
}

func TestAuthInfo_APIKeyAuthMethod(t *testing.T) {
	var mu sync.Mutex
	var captured *maniflex.AuthInfo

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
				auth.APIKeyEntry{Key: "k1", Auth: maniflex.AuthInfo{UserID: "svc-1", Roles: []string{"admin"}}},
			))
			srv.Pipeline.DB.Register(
				captureAuthMiddleware(&mu, &captured),
				maniflex.AtPosition(maniflex.Before),
			)
		},
	})

	s.GET("/minimal_models", map[string]string{"X-API-Key": "k1"})

	mu.Lock()
	got := captured
	mu.Unlock()

	if got == nil {
		t.Fatal("ctx.Auth nil")
	}
	if got.AuthMethod != "api_key" {
		t.Errorf("AuthMethod: want api_key, got %q", got.AuthMethod)
	}
}

func TestAuthInfo_APIKeyCallerSetAuthMethodNotOverridden(t *testing.T) {
	var mu sync.Mutex
	var captured *maniflex.AuthInfo

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
				auth.APIKeyEntry{Key: "k2", Auth: maniflex.AuthInfo{UserID: "svc-2", AuthMethod: "oauth2"}},
			))
			srv.Pipeline.DB.Register(
				captureAuthMiddleware(&mu, &captured),
				maniflex.AtPosition(maniflex.Before),
			)
		},
	})

	s.GET("/minimal_models", map[string]string{"X-API-Key": "k2"})

	mu.Lock()
	got := captured
	mu.Unlock()

	if got == nil {
		t.Fatal("ctx.Auth nil")
	}
	if got.AuthMethod != "oauth2" {
		t.Errorf("AuthMethod: want oauth2 (caller-set), got %q", got.AuthMethod)
	}
}

func TestAuthInfo_IdentityTypeConstants(t *testing.T) {
	if maniflex.IdentityAnonymous != "" {
		t.Errorf("IdentityAnonymous must be the zero value (empty string)")
	}
	if maniflex.IdentityHuman == "" {
		t.Errorf("IdentityHuman must be non-empty")
	}
	if maniflex.IdentityServiceAccount == "" {
		t.Errorf("IdentityServiceAccount must be non-empty")
	}
	var ai maniflex.AuthInfo
	if ai.IdentityType != maniflex.IdentityAnonymous {
		t.Errorf("default AuthInfo.IdentityType should be IdentityAnonymous (zero value)")
	}
}

func TestAuthInfo_NoAuthReturns401(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{minimalModel{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth("secret"))
		},
	})

	s.GET("/minimal_models").AssertStatus(http.StatusUnauthorized)
}
