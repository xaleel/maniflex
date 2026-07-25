package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestCache_AuthenticatedResponsesDefaultToPrivate(t *testing.T) {
	t.Parallel()

	const secret = "cache-test-secret-at-least-32-bytes"
	token := makeJWT(t, secret, "cache-user", []string{"viewer"}, time.Hour)
	authHeader := map[string]string{"Authorization": "Bearer " + token}

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(secret))
			s.Pipeline.Response.Register(
				response.Cache(response.CacheConfig{
					MaxAge: 120,
					Vary:   []string{"Authorization"},
				}),
				maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	id := srv.POST("/users", map[string]any{
		"name": "Private", "email": "private-cache@example.com", "password": "secret",
	}, authHeader).AssertStatus(http.StatusCreated).Data()["id"].(string)

	resp := srv.GET("/users/"+id, authHeader).AssertStatus(http.StatusOK)
	if got := resp.Header.Get("Cache-Control"); got != "private, max-age=120" {
		t.Errorf("authenticated Cache-Control = %q, want private policy", got)
	}
	if strings.Contains(resp.Header.Get("Cache-Control"), "public") {
		t.Errorf("authenticated response enabled shared caching: %q",
			resp.Header.Get("Cache-Control"))
	}
	if !headerListContains(resp.Header.Get("Vary"), "Authorization") {
		t.Errorf("Vary = %q, missing Authorization", resp.Header.Get("Vary"))
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("private response is missing ETag validator")
	}
}

func TestCache_PublicResponsesRequireExplicitOptIn(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				response.Cache(response.CacheConfig{
					Public: true,
					MaxAge: 60,
					Vary:   []string{"Accept-Encoding"},
				}),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	id := srv.MustID(srv.CreateUser("Public", "public-cache@example.com", "viewer"))
	resp := srv.GET("/users/" + id).AssertStatus(http.StatusOK)
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want explicit public policy", got)
	}
	if !headerListContains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary = %q, missing Accept-Encoding", resp.Header.Get("Vary"))
	}
}
