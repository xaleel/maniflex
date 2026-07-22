package realtime_test

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Connection teardown (RT-1) ────────────────────────────────────────────────
//
// A WebSocket connection is served by two goroutines that must die together.
// These tests assert that whichever one notices the peer is gone tears the
// whole connection down, so neither the sibling goroutine nor the socket is
// left behind. They count live goroutines process-wide and so deliberately do
// NOT call t.Parallel() — Go runs every non-parallel test to completion before
// releasing the parallel ones, so nothing else in this package holds a
// connection open while they measure.

const (
	readLoopFrame  = "realtime.(*hubClient).readLoop"
	writeLoopFrame = "realtime.(*hubClient).writeLoop"
)

// countGoroutinesIn reports how many live goroutine stacks mention fn.
func countGoroutinesIn(fn string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), fn)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitForGoroutines polls until exactly want goroutines are running fn, or d
// elapses. It returns the last count observed.
func waitForGoroutines(fn string, want int, d time.Duration) (int, bool) {
	deadline := time.Now().Add(d)
	for {
		got := countGoroutinesIn(fn)
		if got == want {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hardClose drops the TCP connection without sending a close frame — the
// abrupt disconnect a crashed client or a yanked network produces.
func (c *wsClient) hardClose() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.conn.Close()
}

// halfClose shuts down only the client's write side. The server sees EOF on
// its read while the client's receive side stays wide open, so every frame the
// server writes still succeeds.
func (c *wsClient) halfClose(t *testing.T) {
	t.Helper()
	tc, ok := c.conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("halfClose: want *net.TCPConn, got %T", c.conn)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatalf("halfClose: %v", err)
	}
}

func TestHub_AbruptDisconnect_TearsDownBothPumps(t *testing.T) {
	bus := inproc.New()
	// A long ping interval parks writeLoop on an idle select: closing done is
	// then the only thing that can wake it. Pre-fix it sat here for a full
	// PingInterval before even attempting the write that might fail.
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	baseRead := countGoroutinesIn(readLoopFrame)
	baseWrite := countGoroutinesIn(writeLoopFrame)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")
	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite+1, 2*time.Second); !ok {
		t.Fatalf("write pump never started: want %d goroutines in writeLoop, got %d", baseWrite+1, n)
	}

	c.hardClose()

	if n, ok := waitForGoroutines(readLoopFrame, baseRead, 2*time.Second); !ok {
		t.Errorf("read pump leaked: want %d goroutines in readLoop, got %d", baseRead, n)
	}
	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite, 2*time.Second); !ok {
		t.Errorf("write pump leaked after the peer vanished: want %d goroutines in writeLoop, got %d "+
			"(the read pump exited without signalling done, so the writer is parked until the next ping)",
			baseWrite, n)
	}
	if s := hub.Stats(); s.Connections != 0 {
		t.Errorf("connections after disconnect: want 0, got %d", s.Connections)
	}
}

func TestHub_HalfCloseDisconnect_ServerClosesTheSocket(t *testing.T) {
	bus := inproc.New()
	// The short ping interval is what makes this the permanent case: the peer's
	// receive side is open, so every ping the server writes succeeds and no
	// write error will ever end the connection on its own.
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	baseWrite := countGoroutinesIn(writeLoopFrame)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")
	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite+1, 2*time.Second); !ok {
		t.Fatalf("write pump never started: want %d goroutines in writeLoop, got %d", baseWrite+1, n)
	}

	c.halfClose(t)

	// The client keeps reading. The server must close the socket, which shows up
	// here as EOF; pre-fix the stream of keepalive pings never ended.
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, err := c.recvFrame()
		if err == nil {
			continue // a keepalive ping while we wait for the teardown
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("the server never closed the socket after the client half-closed: " +
				"still delivering frames 2s later, so the fd is held open indefinitely")
		}
		break // EOF or reset: the server tore the socket down
	}

	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite, 2*time.Second); !ok {
		t.Errorf("write pump leaked after the client half-closed: want %d goroutines in writeLoop, got %d",
			baseWrite, n)
	}
	if s := hub.Stats(); s.Connections != 0 {
		t.Errorf("connections after half-close: want 0, got %d", s.Connections)
	}
}

// TestHub_ClientCloseFrame_ServerRepliesThenTearsDown guards the ordering
// inside sendClose: the courtesy close frame must reach the client before the
// socket goes away, so a teardown that ran first would surface here as EOF in
// place of the 1000.
func TestHub_ClientCloseFrame_ServerRepliesThenTearsDown(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	baseWrite := countGoroutinesIn(writeLoopFrame)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	c.sendFrame(wsOpcodeClose, []byte{0x03, 0xE8}) // 1000, normal closure

	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatal("no close frame from the server after the client sent one")
	}
	if code != 1000 {
		t.Errorf("close code: want 1000, got %d", code)
	}
	if n, ok := waitForGoroutines(writeLoopFrame, baseWrite, 2*time.Second); !ok {
		t.Errorf("write pump leaked after a graceful close: want %d goroutines in writeLoop, got %d",
			baseWrite, n)
	}
}

// TestHub_IdleClientIsNotTornDown is the over-reach guard: tearing down on any
// pump exit must not tear down a connection whose pumps are simply idle.
func TestHub_IdleClientIsNotTornDown(t *testing.T) {
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	time.Sleep(300 * time.Millisecond) // several ping intervals, no traffic

	publish(t, bus, "thing.created", "thing/1")
	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("an idle-but-live client stopped receiving events")
	}
	if op, _ := msg["op"].(string); op != "event" {
		t.Errorf("op: want %q, got %q", "event", op)
	}
	if s := hub.Stats(); s.Connections != 1 {
		t.Errorf("connections for a live client: want 1, got %d", s.Connections)
	}
}
