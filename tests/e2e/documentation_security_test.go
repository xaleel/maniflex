package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func enableTestAsyncAPI(s *maniflex.Server) {
	s.RealtimeDoc(maniflex.AsyncAPIConfig{
		Title:   "Internal Events",
		Version: "1.0.0",
	})
}

func TestDocumentation_DefaultConfigurationMountsNoSpecifications(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Documentation = maniflex.DocumentationConfig{}
		},
		Middleware: enableTestAsyncAPI,
	})

	srv.GET("/openapi.json").AssertStatus(http.StatusNotFound)
	srv.GET("/asyncapi.json").AssertStatus(http.StatusNotFound)
}

func TestDocumentation_PublicOptInMountsBothWithoutForcingCORS(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Documentation = maniflex.DocumentationConfig{Public: true}
		},
		Middleware: enableTestAsyncAPI,
	})

	for _, path := range []string{"/openapi.json", "/asyncapi.json"} {
		resp := srv.GET(path).AssertStatus(http.StatusOK)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s forced Access-Control-Allow-Origin %q", path, got)
		}
	}
}

func TestDocumentation_SharedAuthPolicyProtectsOpenAPIAndAsyncAPI(t *testing.T) {
	t.Parallel()
	const secret = "documentation-test-secret-32-bytes!"
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Documentation = maniflex.DocumentationConfig{
				Middleware: []maniflex.HTTPMiddleware{
					maniflex.AdaptAuth(
						auth.JWTAuth(secret),
						auth.RequireRole("internal"),
					),
				},
			}
		},
		Middleware: enableTestAsyncAPI,
	})

	viewer := makeJWT(t, secret, "viewer", []string{"viewer"}, time.Hour)
	internal := makeJWT(t, secret, "operator", []string{"internal"}, time.Hour)

	for _, path := range []string{"/openapi.json", "/asyncapi.json"} {
		srv.GET(path).AssertStatus(http.StatusUnauthorized)
		srv.GET(path, map[string]string{
			"Authorization": "Bearer " + viewer,
		}).AssertStatus(http.StatusForbidden)
		srv.GET(path, map[string]string{
			"Authorization": "Bearer " + internal,
		}).AssertStatus(http.StatusOK)
	}
}

func TestDocumentation_ExistingOpenAPIAuthGateStillMountsOnlyOpenAPI(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Documentation = maniflex.DocumentationConfig{}
		},
		Middleware: func(s *maniflex.Server) {
			enableTestAsyncAPI(s)
			s.Pipeline.OpenAPI.Auth.Register(
				func(ctx *maniflex.OpenAPIContext, _ func() error) error {
					ctx.Abort(http.StatusForbidden, "FORBIDDEN", "spec is private")
					return nil
				},
			)
		},
	})

	srv.GET("/openapi.json").AssertStatus(http.StatusForbidden)
	srv.GET("/asyncapi.json").AssertStatus(http.StatusNotFound)
}
