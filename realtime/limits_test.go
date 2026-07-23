package realtime_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Connection and subscription caps (RT-5) ───────────────────────────────────
//
// connN was counted but never enforced: every accept path incremented it
// unconditionally, so an unbounded number of WebSocket + SSE connections could
// pile up (a trivial exhaustion DoS, worsened by the RT-1 leak). These tests
// cover MaxConnections (a shared WS+SSE cap answered with 503) and
// MaxSubscriptionsPerConn (a per-WebSocket subscription cap).

// waitForConnections polls Stats().Connections until it equals want or d
// elapses. Disconnect decrements are asynchronous — remove() runs when the
// pumps exit — so a poll is the honest way to observe them.
func waitForConnections(h *realtime.Hub, want int, d time.Duration) (int, bool) {
	deadline := time.Now().Add(d)
	for {
		got := h.Stats().Connections
		if got == want {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHub_MaxConnections_RefusesOverTheCap(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, MaxConnections: 2})
	ts := newHubTestServer(t, hub)

	dialWS(t, ts, "/ws")
	dialWS(t, ts, "/ws")
	if n, ok := waitForConnections(hub, 2, 2*time.Second); !ok {
		t.Fatalf("two connections should fill the cap: want 2, got %d", n)
	}

	// The third is refused with a clean 503, not a hijacked-then-dropped socket.
	if status := dialWSExpectStatus(t, ts, "/ws"); status != http.StatusServiceUnavailable {
		t.Errorf("third WS upgrade at the cap: want 503, got %d", status)
	}
	if n := hub.Stats().Connections; n != 2 {
		t.Errorf("a refused connection must not be counted: want 2, got %d", n)
	}
}

func TestHub_MaxConnections_SharedAcrossWebSocketAndSSE(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, MaxConnections: 2})
	ts := newHubTestServer(t, hub)

	dialWS(t, ts, "/ws")
	dialSSE(t, ts, "/sse?subscribe=*")
	if n, ok := waitForConnections(hub, 2, 2*time.Second); !ok {
		t.Fatalf("one WS + one SSE should fill the cap: want 2, got %d", n)
	}

	// Both transports see the same full hub.
	if status := dialWSExpectStatus(t, ts, "/ws"); status != http.StatusServiceUnavailable {
		t.Errorf("WS at the shared cap: want 503, got %d", status)
	}
	if status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*"); status != http.StatusServiceUnavailable {
		t.Errorf("SSE at the shared cap: want 503, got %d", status)
	}
}

// TestHub_MaxConnections_SlotFreedOnDisconnect proves the reservation is
// balanced: a disconnect must return the slot, or the cap would ratchet down to
// zero over a server's lifetime and refuse everyone.
func TestHub_MaxConnections_SlotFreedOnDisconnect(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, MaxConnections: 1})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	if n, ok := waitForConnections(hub, 1, 2*time.Second); !ok {
		t.Fatalf("one connection should fill a cap of 1: want 1, got %d", n)
	}
	if status := dialWSExpectStatus(t, ts, "/ws"); status != http.StatusServiceUnavailable {
		t.Fatalf("second WS at a cap of 1: want 503, got %d", status)
	}

	c.closeGracefully()
	if n, ok := waitForConnections(hub, 0, 2*time.Second); !ok {
		t.Fatalf("the slot was not freed on disconnect: want 0, got %d", n)
	}

	// With the slot back, a new connection is admitted.
	dialWS(t, ts, "/ws")
	if n, ok := waitForConnections(hub, 1, 2*time.Second); !ok {
		t.Errorf("a freed slot should admit a new connection: want 1, got %d", n)
	}
}

// TestHub_MaxConnections_UnsetIsUnlimited is the over-reach guard for the
// connection cap: 0 must keep the previous unbounded behaviour.
func TestHub_MaxConnections_UnsetIsUnlimited(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus}) // MaxConnections unset
	ts := newHubTestServer(t, hub)

	const n = 12
	for range n {
		dialWS(t, ts, "/ws")
	}
	if got, ok := waitForConnections(hub, n, 2*time.Second); !ok {
		t.Errorf("unlimited cap should admit all %d: got %d", n, got)
	}
}

func TestHub_MaxSubscriptionsPerConn_RefusesOverTheCap(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, MaxSubscriptionsPerConn: 3})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")

	subIDs := make([]string, 0, 3)
	for i := range 3 {
		ack := c.subscribe(patternFor(i))
		subIDs = append(subIDs, ack["subId"].(string))
	}

	// The fourth subscribe is refused, and the connection stays open.
	c.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"extra.*"}})
	msg := c.mustRecvOp("error")
	if code, _ := msg["code"].(string); code != "TOO_MANY_SUBSCRIPTIONS" {
		t.Errorf("over-cap subscribe: want code TOO_MANY_SUBSCRIPTIONS, got %q", code)
	}

	// Unsubscribing frees a slot, so the next subscribe succeeds.
	c.sendJSON(map[string]any{"op": "unsubscribe", "subId": subIDs[0]})
	c.mustRecvOp("unsubscribed")
	c.subscribe("afterfree.*") // asserts an ack
}

// TestHub_MaxSubscriptionsPerConn_UnsetIsUnlimited is the over-reach guard for
// the subscription cap.
func TestHub_MaxSubscriptionsPerConn_UnsetIsUnlimited(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus}) // cap unset
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	for i := range 20 {
		c.subscribe(patternFor(i)) // each asserts an ack
	}
}

// patternFor returns a distinct subscribe pattern per index.
func patternFor(i int) string {
	return "topic" + string(rune('a'+i%26)) + ".*"
}
