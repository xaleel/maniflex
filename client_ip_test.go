package maniflex

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP_StripsTheEphemeralPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	ctx := &ServerContext{Request: req}

	if got := ctx.ClientIP(); got != "203.0.113.7" {
		t.Errorf("ClientIP() = %q, want %q — the port differs on every connection, so keying on it gives each connection its own bucket", got, "203.0.113.7")
	}
}

func TestClientIP_KeepsAnIPv6Address(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8::1]:54321"
	ctx := &ServerContext{Request: req}

	if got := ctx.ClientIP(); got != "2001:db8::1" {
		t.Errorf("ClientIP() = %q, want %q", got, "2001:db8::1")
	}
}

func TestClientIP_LeavesAResolvedBareAddressAlone(t *testing.T) {
	// The trusted-proxy resolver rewrites RemoteAddr to a bare IP, which has no
	// port to split. Splitting must not mangle it or return empty.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.4"
	ctx := &ServerContext{Request: req}

	if got := ctx.ClientIP(); got != "198.51.100.4" {
		t.Errorf("ClientIP() = %q, want the address unchanged", got)
	}
}

func TestClientIP_IsEmptyWithoutARequest(t *testing.T) {
	ctx := &ServerContext{}
	if got := ctx.ClientIP(); got != "" {
		t.Errorf("ClientIP() = %q, want empty", got)
	}
}
