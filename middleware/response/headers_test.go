package response_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
