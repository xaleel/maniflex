package response_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/response"
)

// SEC-6: CORSHeaders must never apply a permissive wildcard by accident. Called
// with no origins it panics at construction instead of defaulting to "*".
func TestCORSHeaders_PanicsWithoutOrigins(t *testing.T) {
	assertPanics(t, "CORSHeaders()", func() { response.CORSHeaders() })
	assertPanics(t, "CORSHeadersWithConfig{}", func() {
		response.CORSHeadersWithConfig(response.CORSConfig{})
	})
}

// SEC-6: "*" together with credentials is invalid per the Fetch spec (browsers
// reject it), so it is refused at construction rather than silently emitted.
func TestCORSHeaders_PanicsOnWildcardWithCredentials(t *testing.T) {
	assertPanics(t, `["*"] + AllowCredentials`, func() {
		response.CORSHeadersWithConfig(response.CORSConfig{
			AllowOrigins:     []string{"*"},
			AllowCredentials: true,
		})
	})
}

// Explicit origins — including an intentional public "*" — are valid and must
// construct without panicking.
func TestCORSHeaders_ValidConfigsDoNotPanic(t *testing.T) {
	assertNoPanic(t, `CORSHeaders("*")`, func() { response.CORSHeaders("*") })
	assertNoPanic(t, "explicit origin", func() { response.CORSHeaders("https://app.example.com") })
	assertNoPanic(t, "origin + credentials", func() {
		response.CORSHeadersWithConfig(response.CORSConfig{
			AllowOrigins:     []string{"https://app.example.com"},
			AllowCredentials: true,
		})
	})
}

func TestCORSHeaders_ValidPreflightReturns204BeforeNext(t *testing.T) {
	t.Parallel()
	const origin = "https://app.example.com"
	called := false
	handler := response.CORSHeadersWithConfig(response.CORSConfig{
		AllowOrigins:     []string{origin},
		AllowCredentials: true,
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/docs/42", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, If-Match")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if called {
		t.Fatal("valid preflight reached the protected route")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("allow origin = %q, want %q", got, origin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("allow credentials = %q, want true", got)
	}
	for _, want := range []string{"Authorization", "Content-Type", "If-Match"} {
		if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, want) {
			t.Errorf("allow headers %q does not contain %q", got, want)
		}
	}
	for _, want := range []string{http.MethodHead, http.MethodPatch} {
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, want) {
			t.Errorf("allow methods %q does not contain %q", got, want)
		}
	}
	for _, want := range []string{"ETag", "X-Request-Id"} {
		if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, want) {
			t.Errorf("expose headers %q does not contain %q", got, want)
		}
	}
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	for _, want := range []string{
		"Origin",
		"Access-Control-Request-Method",
		"Access-Control-Request-Headers",
	} {
		if !strings.Contains(vary, want) {
			t.Errorf("Vary = %q, missing %q", vary, want)
		}
	}
}

func TestCORSHeaders_RejectsInvalidPreflightPolicy(t *testing.T) {
	t.Parallel()
	middleware := response.CORSHeaders("https://app.example.com")

	tests := []struct {
		name    string
		origin  string
		method  string
		headers string
		status  int
		code    string
	}{
		{
			name:   "origin",
			origin: "https://evil.example.com",
			method: http.MethodPatch,
			status: http.StatusForbidden,
			code:   "CORS_ORIGIN_DENIED",
		},
		{
			name:   "method",
			origin: "https://app.example.com",
			method: http.MethodPut,
			status: http.StatusMethodNotAllowed,
			code:   "CORS_METHOD_DENIED",
		},
		{
			name:    "header",
			origin:  "https://app.example.com",
			method:  http.MethodPatch,
			headers: "X-Internal-Admin",
			status:  http.StatusForbidden,
			code:    "CORS_HEADER_DENIED",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			handler := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodOptions, "/api/docs/42", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", tc.method)
			if tc.headers != "" {
				req.Header.Set("Access-Control-Request-Headers", tc.headers)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if called {
				t.Fatal("rejected preflight reached the protected route")
			}
			if !strings.Contains(rec.Body.String(), tc.code) {
				t.Errorf("body = %q, want code %s", rec.Body.String(), tc.code)
			}
			if tc.name == "origin" && rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Error("rejected origin received Access-Control-Allow-Origin")
			}
		})
	}
}

func TestCORSHeaders_PlainOptionsContinuesToRoute(t *testing.T) {
	t.Parallel()
	called := false
	handler := response.CORSHeaders("https://app.example.com")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	req := httptest.NewRequest(http.MethodOptions, "/api/docs", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("plain OPTIONS was mistaken for a CORS preflight")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestCache_DefaultPolicyIsPrivate(t *testing.T) {
	t.Parallel()
	ctx, rec := runCache(t, response.CacheConfig{}, maniflex.OpRead, http.StatusOK)

	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=0" {
		t.Errorf("Cache-Control = %q, want private default", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("private storable response is missing ETag")
	}
	if ctx.Response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode)
	}
}

func TestCache_PublicRequiresExplicitPolicyAndMergesVary(t *testing.T) {
	t.Parallel()
	cfg := response.CacheConfig{
		Public: true,
		MaxAge: 300,
		Vary:   []string{"authorization", "Origin", "AUTHORIZATION"},
	}
	_, rec := runCacheWithHeader(t, cfg, maniflex.OpList, http.StatusOK, http.Header{
		"Vary": []string{"Accept-Encoding, Origin"},
	})

	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want explicit public policy", got)
	}
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	for _, want := range []string{"Accept-Encoding", "Origin", "Authorization"} {
		if !headerValueContains(vary, want) {
			t.Errorf("Vary = %q, missing %q", vary, want)
		}
	}
	if countHeaderValue(vary, "Origin") != 1 || countHeaderValue(vary, "Authorization") != 1 {
		t.Errorf("Vary values were not deduplicated: %q", vary)
	}
}

func TestCache_VaryWildcardSupersedesHeaderNames(t *testing.T) {
	t.Parallel()

	_, existingWildcard := runCacheWithHeader(t,
		response.CacheConfig{Vary: []string{"Authorization"}},
		maniflex.OpRead,
		http.StatusOK,
		http.Header{"Vary": []string{"*"}},
	)
	if got := existingWildcard.Header().Get("Vary"); got != "*" {
		t.Errorf("existing wildcard Vary = %q, want *", got)
	}

	_, configuredWildcard := runCacheWithHeader(t,
		response.CacheConfig{Vary: []string{"*"}},
		maniflex.OpRead,
		http.StatusOK,
		http.Header{"Vary": []string{"Origin"}},
	)
	if got := configuredWildcard.Header().Get("Vary"); got != "*" {
		t.Errorf("configured wildcard Vary = %q, want *", got)
	}
}

func TestCache_NoStoreDisablesValidators(t *testing.T) {
	t.Parallel()
	ctx, rec := runCacheWithHeader(t,
		response.CacheConfig{NoStore: true},
		maniflex.OpRead,
		http.StatusOK,
		http.Header{"If-None-Match": []string{`"*"`}},
	)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("no-store response received ETag %q", got)
	}
	if ctx.Response.StatusCode != http.StatusOK {
		t.Errorf("no-store response honored If-None-Match: status = %d, want 200",
			ctx.Response.StatusCode)
	}
}

func TestCache_PrivatePolicyAlsoProtectsReadErrors(t *testing.T) {
	t.Parallel()
	_, rec := runCache(t, response.CacheConfig{MaxAge: 60}, maniflex.OpRead, http.StatusNotFound)
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=60" {
		t.Errorf("Cache-Control = %q, want private policy on cacheable 404", got)
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("error response must not receive a representation ETag")
	}
}

func TestCache_IsNoOpForWrites(t *testing.T) {
	t.Parallel()
	_, rec := runCache(t, response.CacheConfig{Public: true, MaxAge: 60},
		maniflex.OpCreate, http.StatusCreated)
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("write response Cache-Control = %q, want empty", got)
	}
}

func TestCache_RejectsContradictoryPolicy(t *testing.T) {
	tests := []struct {
		name string
		cfg  response.CacheConfig
	}{
		{"public and private", response.CacheConfig{Public: true, Private: true}},
		{"public and no-store", response.CacheConfig{Public: true, NoStore: true}},
		{"no-store with max-age", response.CacheConfig{NoStore: true, MaxAge: 1}},
		{"negative max-age", response.CacheConfig{MaxAge: -1}},
		{"invalid vary", response.CacheConfig{Vary: []string{"Bad Header"}}},
		{"wildcard vary mix", response.CacheConfig{Vary: []string{"*", "Authorization"}}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assertPanics(t, tc.name, func() { response.Cache(tc.cfg) })
		})
	}
}

func runCache(t *testing.T, cfg response.CacheConfig, operation maniflex.Operation, status int) (*maniflex.ServerContext, *httptest.ResponseRecorder) {
	t.Helper()
	return runCacheWithHeader(t, cfg, operation, status, nil)
}

func runCacheWithHeader(t *testing.T, cfg response.CacheConfig, operation maniflex.Operation, status int, header http.Header) (*maniflex.ServerContext, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/docs/42", nil)
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	if values := header.Values("Vary"); len(values) > 0 {
		rec.Header()["Vary"] = append([]string(nil), values...)
	}
	ctx := &maniflex.ServerContext{
		Request:   req,
		Writer:    rec,
		Operation: operation,
	}
	err := response.Cache(cfg)(ctx, func() error {
		ctx.Response = &maniflex.APIResponse{
			StatusCode: status,
			Data:       map[string]any{"id": "42", "owner": "user-a"},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cache middleware: %v", err)
	}
	return ctx, rec
}

func headerValueContains(list, want string) bool {
	return countHeaderValue(list, want) > 0
}

func countHeaderValue(list, want string) int {
	count := 0
	for _, value := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			count++
		}
	}
	return count
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

func assertNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: unexpected panic: %v", name, r)
		}
	}()
	fn()
}
