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

// csrfModel is a tiny model used by the CSRF e2e tests.
type csrfModel struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

func newCSRFServer(t *testing.T, register func(s *maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{csrfModel{}},
		Middleware: func(s *maniflex.Server) {
			if register != nil {
				register(s)
			}
		},
	})
}

func TestCSRF_DoubleSubmit_SafeMethodIssuesCookie(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode: auth.CSRFDoubleSubmit,
		}))
	})

	resp := s.GET("/csrf_models")
	resp.AssertStatus(http.StatusOK)

	var token string
	for _, c := range resp.Header.Values("Set-Cookie") {
		if cookieNameIs(c, "csrf_token") {
			token = cookieValue(c)
		}
	}
	if token == "" {
		t.Fatalf("expected csrf_token Set-Cookie on safe-method response; headers=%v", resp.Header)
	}
}

func TestCSRF_DoubleSubmit_RejectsUnsafeWithoutToken(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode: auth.CSRFDoubleSubmit,
		}))
	})

	resp := s.POST("/csrf_models", map[string]any{"name": "x"})
	resp.AssertStatus(http.StatusForbidden)
	if got := resp.ErrorCode(); got != "CSRF_COOKIE_MISSING" && got != "CSRF_TOKEN_MISSING" {
		t.Fatalf("unexpected error code: %s", got)
	}
}

func TestCSRF_DoubleSubmit_AcceptsMatchingCookieAndHeader(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode: auth.CSRFDoubleSubmit,
		}))
	})

	const token = "test-token-value"
	headers := map[string]string{
		"Cookie":       "csrf_token=" + token,
		"X-CSRF-Token": token,
	}
	resp := s.POST("/csrf_models", map[string]any{"name": "x"}, headers)
	resp.AssertStatus(http.StatusCreated)
}

func TestCSRF_DoubleSubmit_RejectsMismatch(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode: auth.CSRFDoubleSubmit,
		}))
	})

	headers := map[string]string{
		"Cookie":       "csrf_token=aaa",
		"X-CSRF-Token": "bbb",
	}
	resp := s.POST("/csrf_models", map[string]any{"name": "x"}, headers)
	resp.AssertStatus(http.StatusForbidden)
	if got := resp.ErrorCode(); got != "CSRF_TOKEN_MISMATCH" {
		t.Fatalf("expected CSRF_TOKEN_MISMATCH, got %s", got)
	}
}

func TestCSRF_SkipBearerLetsRequestThrough(t *testing.T) {
	// Default behaviour: bearer-authenticated requests bypass CSRF checks
	// because EnforceBearer is false (the safe zero-value default).
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode: auth.CSRFDoubleSubmit,
		}))
	})

	headers := map[string]string{"Authorization": "Bearer some.jwt.value"}
	resp := s.POST("/csrf_models", map[string]any{"name": "x"}, headers)
	resp.AssertStatus(http.StatusCreated)
}

// Roadmap §11B.2: callers passing any options field (e.g. just a Mode) used
// to silently lose the SkipBearer=true default — bearer-authenticated API
// callers started getting 403. After the rename, the zero value is the safe
// default and the regression cannot happen.
func TestCSRF_DefaultEnforceBearerIsFalseEvenWhenOtherFieldsSet(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode:           auth.CSRFDoubleSubmit,
			AllowedOrigins: []string{"https://example.com"},
		}))
	})

	headers := map[string]string{"Authorization": "Bearer some.jwt.value"}
	s.POST("/csrf_models", map[string]any{"name": "y"}, headers).
		AssertStatus(http.StatusCreated)
}

func TestCSRF_OriginAllowlist(t *testing.T) {
	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode:           auth.CSRFDoubleSubmit,
			AllowedOrigins: []string{"*.example.com"},
		}))
	})

	const token = "tok"
	good := map[string]string{
		"Origin":       "https://admin.example.com",
		"Cookie":       "csrf_token=" + token,
		"X-CSRF-Token": token,
	}
	s.POST("/csrf_models", map[string]any{"name": "ok"}, good).
		AssertStatus(http.StatusCreated)

	bad := map[string]string{
		"Origin":       "https://evil.attacker.com",
		"Cookie":       "csrf_token=" + token,
		"X-CSRF-Token": token,
	}
	resp := s.POST("/csrf_models", map[string]any{"name": "nope"}, bad)
	resp.AssertStatus(http.StatusForbidden)
	if got := resp.ErrorCode(); got != "CSRF_ORIGIN_REJECTED" {
		t.Fatalf("expected CSRF_ORIGIN_REJECTED, got %s", got)
	}
}

func TestCSRF_SignedToken_Mode(t *testing.T) {
	const secret = "csrf-signing-secret"
	const jwtSecret = "jwt-secret"

	s := newCSRFServer(t, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(auth.JWTAuth(jwtSecret))
		srv.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			Mode:          auth.CSRFSignedToken,
			Secret:        secret,
			EnforceBearer: true,
		}))
	})

	token := makeJWTClaims(t, jwtSecret, map[string]any{
		"sub": "u-1",
		"jti": "session-42",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	csrfTok := auth.SignedCSRFToken("session-42", secret)

	good := map[string]string{
		"Authorization": "Bearer " + token,
		"X-CSRF-Token":  csrfTok,
	}
	s.POST("/csrf_models", map[string]any{"name": "ok"}, good).
		AssertStatus(http.StatusCreated)

	bad := map[string]string{
		"Authorization": "Bearer " + token,
		"X-CSRF-Token":  "wrong",
	}
	s.POST("/csrf_models", map[string]any{"name": "bad"}, bad).
		AssertStatus(http.StatusForbidden)
}

// ── tiny cookie parser used only by these tests ───────────────────────────────

func cookieNameIs(setCookie, name string) bool {
	eq := strings.IndexByte(setCookie, '=')
	return eq >= 0 && setCookie[:eq] == name
}

func cookieValue(setCookie string) string {
	eq := strings.IndexByte(setCookie, '=')
	if eq < 0 {
		return ""
	}
	rest := setCookie[eq+1:]
	if semi := strings.IndexByte(rest, ';'); semi >= 0 {
		return rest[:semi]
	}
	return rest
}
