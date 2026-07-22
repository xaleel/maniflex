package realtime_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Read deadline (RT-2) ──────────────────────────────────────────────────────
//
// The hub pinged but never enforced an answer, so a peer that stopped
// responding without closing — the half-open TCP connection a network
// partition leaves behind — kept its read pump blocked in recvFrame forever.
// These tests use a client that deliberately does NOT answer pings, which is
// what recvCloseCode and recvNoPong give: they read frames and reply to
// nothing.

// recvOpNoPong reads frames until a text frame carrying the wanted "op"
// arrives, or d elapses. Unlike wsClient.recvTimeout it never answers a ping,
// so the connection is kept alive only by whatever else the test sends.
func recvOpNoPong(t *testing.T, c *wsClient, wantOp string, d time.Duration) bool {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		f, err := c.recvFrame()
		if err != nil {
			return false
		}
		if f.opcode != wsOpcodeText && f.opcode != wsOpcodeBinary {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(f.payload, &msg); err != nil {
			continue
		}
		if op, _ := msg["op"].(string); op == wantOp {
			return true
		}
	}
}

func TestHub_SilentClient_ReapedByReadDeadline(t *testing.T) {
	bus := inproc.New()
	// 50ms cadence → a 100ms read deadline by default.
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	baseWrite := countGoroutinesIn(writeLoopFrame)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// From here the client reads but answers nothing — the server's pings go
	// unanswered, which is exactly what a half-open connection looks like from
	// the server's side.
	//
	// Teardown is asserted before the close code so the two failures stay
	// distinguishable: a deadline that never fires fails both, a teardown that
	// forgets to announce itself fails only the second.
	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite, 2*time.Second); !ok {
		t.Fatalf("a client that never answered a ping was never reaped: want %d goroutines in "+
			"writeLoop, got %d — the read pump is blocked in recvFrame with no deadline, which "+
			"no amount of pinging can end", baseWrite, n)
	}
	if s := hub.Stats(); s.Connections != 0 {
		t.Errorf("connections after reaping a dead peer: want 0, got %d", s.Connections)
	}
	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatal("no close frame before the socket went away: the connection was dropped " +
			"without telling the client why")
	}
	if code != 1001 {
		t.Errorf("close code on read-deadline expiry: want 1001, got %d", code)
	}
}

// TestHub_ReadTimeout_OverridesTheDerivedDefault pins that the field is read
// rather than the 2×PingInterval derivation: the ping cadence here is a minute,
// so no ping is even sent inside the window and the derived deadline would be
// two minutes.
func TestHub_ReadTimeout_OverridesTheDerivedDefault(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:          bus,
		PingInterval: time.Minute,
		ReadTimeout:  150 * time.Millisecond,
	})
	ts := newHubTestServer(t, hub)

	baseWrite := countGoroutinesIn(writeLoopFrame)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite, 2*time.Second); !ok {
		t.Fatalf("the configured ReadTimeout was not applied: want %d goroutines in writeLoop, "+
			"got %d 2s after a 150ms deadline", baseWrite, n)
	}
	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatal("no close frame before the socket went away")
	}
	if code != 1001 {
		t.Errorf("close code on read-deadline expiry: want 1001, got %d", code)
	}
}

// TestHub_AnyInboundFrameRefreshesTheReadDeadline covers the other half of the
// contract: the deadline tracks inbound traffic of any kind, not pongs
// specifically. This client ignores every server ping and stays connected
// purely on its own application-level messages.
func TestHub_AnyInboundFrameRefreshesTheReadDeadline(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// 400ms is four times the 100ms deadline, so a client that only refreshed
	// on pongs would have been reaped several times over.
	for range 10 {
		c.sendJSON(map[string]any{"op": "ping"})
		time.Sleep(40 * time.Millisecond)
	}

	if s := hub.Stats(); s.Connections != 1 {
		t.Fatalf("connections for a client sending its own traffic: want 1, got %d", s.Connections)
	}
	publish(t, bus, "thing.created", "thing/1")
	if !recvOpNoPong(t, c, "event", 2*time.Second) {
		t.Error("a client that never ponged but kept sending was disconnected anyway")
	}
}

func TestHub_ReadTimeoutDisabled_SilentClientSurvives(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:          bus,
		PingInterval: 50 * time.Millisecond,
		ReadTimeout:  realtime.ReadTimeoutDisabled,
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// Ten ping intervals — five default deadline windows — of complete silence.
	time.Sleep(500 * time.Millisecond)

	if s := hub.Stats(); s.Connections != 1 {
		t.Fatalf("ReadTimeoutDisabled still reaped a silent client: connections want 1, got %d",
			s.Connections)
	}
	publish(t, bus, "thing.created", "thing/1")
	if !recvOpNoPong(t, c, "event", 2*time.Second) {
		t.Error("a silent client was disconnected despite ReadTimeoutDisabled")
	}
}
