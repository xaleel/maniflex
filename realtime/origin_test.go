package realtime_test

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── SSE Origin gate (RT-4) ────────────────────────────────────────────────────
//
// SSEHandler ignored HubConfig.Origins entirely, so a cross-origin page could
// open an EventSource against a cookie-authenticated hub and (with a permissive
// app CORS config) read the stream, while the WebSocket handler already gated
// on Origin. These tests cover the gate and the one asymmetry it must have: a
// same-origin EventSource sends no Origin header, so an absent Origin is
// allowed here where it is refused on the WS handshake.

func sseOrigin(t *testing.T, origin string) http.Header {
	t.Helper()
	h := make(http.Header)
	h.Set("Origin", origin)
	return h
}

func TestSSE_Origins_ForbiddenOriginRejected(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*",
		sseOrigin(t, "https://evil.example.com"))
	if status != http.StatusForbidden {
		t.Errorf("cross-origin EventSource: want 403, got %d", status)
	}
}

func TestSSE_Origins_AllowedOriginConnects(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	// dialSSE asserts a 200 + text/event-stream, so reaching it is the pass.
	dialSSE(t, ts, "/sse?subscribe=*", sseOrigin(t, "https://myapp.example.com"))
}

// TestSSE_Origins_AbsentOriginAllowed is the load-bearing asymmetry: a
// same-origin EventSource omits Origin, and refusing that would break every
// ordinary SSE client the instant Origins is set. The WS gate refuses the same
// case on purpose — TestSSE_vs_WS_AbsentOriginDiffer pins the contrast.
func TestSSE_Origins_AbsentOriginAllowed(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	// No Origin header at all — the same-origin browser case.
	dialSSE(t, ts, "/sse?subscribe=*")
}

func TestSSE_vs_WS_AbsentOriginDiffer(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	// SSE with no Origin: allowed.
	if status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*"); status != http.StatusOK {
		t.Errorf("SSE with no Origin: want 200, got %d", status)
	}
	// WS with no Origin: refused, because RFC 6455 requires a browser to send
	// one, so its absence means the caller is not the browser the allowlist is
	// protecting.
	if status := dialWSExpectStatus(t, ts, "/ws"); status == http.StatusSwitchingProtocols {
		t.Error("WS with no Origin should not get 101 when Origins is set")
	}
}

// TestSSE_Origins_UnsetAllowsAll guards the zero-config default: with no
// Origins configured every Origin, cross-origin included, still connects.
func TestSSE_Origins_UnsetAllowsAll(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	dialSSE(t, ts, "/sse?subscribe=*", sseOrigin(t, "https://anywhere.example.com"))
}
