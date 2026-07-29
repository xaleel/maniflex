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

func TestCORSPreflightRunsBeforeJWTAndSupportsOptimisticLocking(t *testing.T) {
	t.Parallel()

	const (
		origin = "https://app.example.com"
		secret = "cors-test-secret-at-least-32-bytes"
	)
	token := makeJWT(t, secret, "browser-user", []string{"editor"}, time.Hour)
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			optDoc{},
			maniflex.ModelConfig{OptimisticLock: true},
		},
		Config: func(cfg *maniflex.Config) {
			cfg.HTTPMiddlewares = append(cfg.HTTPMiddlewares,
				response.CORSHeadersWithConfig(response.CORSConfig{
					AllowOrigins:     []string{origin},
					AllowCredentials: true,
				}),
			)
		},
		Middleware: func(s *maniflex.Server) {
			// Applying JWT to every operation is deliberate: a browser preflight
			// carries no Authorization value and would be rejected if it reached
			// the Auth pipeline.
			s.Pipeline.Auth.Register(auth.JWTAuth(secret))
			s.Pipeline.Response.Register(
				response.Cache(response.CacheConfig{}),
				maniflex.ForModel("optDoc"),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	preflight := srv.Do(http.MethodOptions, srv.APIPath("/opt_docs/example"), nil,
		map[string]string{
			"Origin":                         origin,
			"Access-Control-Request-Method":  http.MethodPatch,
			"Access-Control-Request-Headers": "Authorization, Content-Type, If-Match",
		})
	preflight.AssertStatus(http.StatusNoContent)
	if len(preflight.Body) != 0 {
		t.Errorf("preflight body = %q, want empty", preflight.Body)
	}
	assertCORSBrowserHeaders(t, preflight.Header, origin)
	for _, want := range []string{"Authorization", "Content-Type", "If-Match"} {
		if !headerListContains(preflight.Header.Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %q",
				preflight.Header.Get("Access-Control-Allow-Headers"), want)
		}
	}

	browserHeaders := map[string]string{
		"Origin":        origin,
		"Authorization": "Bearer " + token,
	}
	id := srv.POST("/opt_docs", map[string]any{"title": "original"}, browserHeaders).
		AssertStatus(http.StatusCreated).
		Data()["id"].(string)

	read := srv.GET("/opt_docs/"+id, browserHeaders).AssertStatus(http.StatusOK)
	assertCORSBrowserHeaders(t, read.Header, origin)
	if !headerListContains(read.Header.Get("Access-Control-Expose-Headers"), "ETag") {
		t.Errorf("Access-Control-Expose-Headers = %q, missing ETag",
			read.Header.Get("Access-Control-Expose-Headers"))
	}
	etag := read.Header.Get("ETag")
	if etag == "" {
		t.Fatal("authenticated browser GET did not return an ETag")
	}

	updateHeaders := map[string]string{
		"Origin":        origin,
		"Authorization": "Bearer " + token,
		"If-Match":      etag,
	}
	updated := srv.PATCH("/opt_docs/"+id, map[string]any{"title": "updated"}, updateHeaders).
		AssertStatus(http.StatusOK)
	assertCORSBrowserHeaders(t, updated.Header, origin)
}

// BUG-12: an OPTIONS request with an Origin is not necessarily a CORS
// preflight. Without Access-Control-Request-Method it must continue to the
// generated OPTIONS route, preserving that route's 204/Allow/empty-body
// contract while CORS adds the ordinary cross-origin response headers.
func TestCORSPlainOptionsUsesGeneratedRouteContract(t *testing.T) {
	t.Parallel()

	const origin = "https://app.example.com"
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.HTTPMiddlewares = append(cfg.HTTPMiddlewares,
				response.CORSHeaders(origin),
			)
		},
	})

	options := srv.Do(http.MethodOptions, srv.APIPath("/users"), nil,
		map[string]string{"Origin": origin})
	options.AssertStatus(http.StatusNoContent)
	if len(options.Body) != 0 {
		t.Errorf("OPTIONS body = %q, want empty", options.Body)
	}
	if got := options.Header.Get("Allow"); got != "GET, HEAD, POST, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, POST, OPTIONS")
	}
	if got := options.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
}

func assertCORSBrowserHeaders(t *testing.T, header http.Header, origin string) {
	t.Helper()
	if got := header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	if header.Get("X-Request-Id") == "" {
		t.Error("CORS response is missing X-Request-Id")
	}
}

func headerListContains(list, want string) bool {
	for _, value := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
