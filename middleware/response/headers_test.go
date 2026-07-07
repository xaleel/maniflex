package response_test

import (
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
