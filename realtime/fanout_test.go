package realtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Fan-out head-of-line blocking (RT-3) ──────────────────────────────────────
//
// Fan-out walks every client from the single bus-subscriber goroutine, so a
// wait on one client is a wait imposed on all of them. These tests saturate one
// client and then measure how long a healthy one waits for its next event.
//
// The saturating flood and the probes are published to different patterns and
// in that order, so the probes queue on the bus behind the whole flood: any
// stall the flood causes is paid before the first probe is even considered,
// which makes the measurement independent of the order clients.Range happens to
// visit clients in.

// floodEvents publishes n events of eventType with an 8 KiB payload — large
// enough that a client which is not reading has its TCP receive window fill,
// which is what makes its outbound channel back up.
func floodEvents(t *testing.T, bus events.Publisher, eventType string, n int) {
	t.Helper()
	blob := make([]byte, 8*1024)
	for i := range blob {
		blob[i] = 'x'
	}
	data, err := json.Marshal(map[string]string{"blob": string(blob)})
	if err != nil {
		t.Fatalf("floodEvents: marshal: %v", err)
	}
	for range n {
		if err := bus.Publish(context.Background(), events.Event{
			ID: "flood-id", Source: "/test", Type: eventType, Subject: "x/1", Data: data,
		}); err != nil {
			t.Fatalf("floodEvents: publish: %v", err)
		}
	}
}

// drainText reports every text frame the client receives on the returned
// channel until the connection ends.
func drainText(c *wsClient) <-chan struct{} {
	got := make(chan struct{}, 64)
	go func() {
		for {
			f, err := c.recvFrame()
			if err != nil {
				return
			}
			if f.opcode == wsOpcodeText {
				select {
				case got <- struct{}{}:
				default:
				}
			}
		}
	}()
	return got
}

func TestHub_SlowClientDoesNotDelayOthers(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:          bus,
		SendTimeout:  3 * time.Second, // ignored now; this was the stall length
		PingInterval: time.Minute,     // keep the heartbeat out of the measurement
	})
	ts := newHubTestServer(t, hub)

	// The slow client matches everything and never reads again after
	// subscribing. The fast client matches only the probes, so the flood never
	// touches its buffer and the test stays about the slow one.
	slow := dialWS(t, ts, "/ws")
	slow.subscribe("*")

	fast := dialWS(t, ts, "/ws")
	fast.subscribe("probe.*")
	probes := drainText(fast)

	// The default 64-deep SendBuffer is load-bearing here, and a shallow one
	// makes this test lie. With SendBuffer 1 the buffer fills transiently while
	// the socket is still draining fine, so the kick lands at a moment when the
	// write pump is parked rather than mid-write — and a kick that blocks on
	// the write mutex then goes undetected. Enough payload to fill the socket
	// AND the 64-deep queue behind it puts the pump reliably inside conn.Write.
	floodEvents(t, bus, "flood.event", 256)

	start := time.Now()
	for range 3 {
		publish(t, bus, "probe.tick", "probe/1")
	}
	for i := range 3 {
		select {
		case <-probes:
		case <-time.After(1500 * time.Millisecond):
			t.Fatalf("a healthy client got only %d of 3 events in %v: fan-out is blocked "+
				"waiting on a client that cannot keep up, and every other client waits with it",
				i, time.Since(start))
		}
	}

	// Anti-vacuity: if the flood never actually saturated the slow client there
	// was nothing to block on, and the timing above would prove nothing.
	if s := hub.Stats(); s.Kicked == 0 {
		t.Error("the slow client was never kicked, so it was never saturated: this test " +
			"measured an unobstructed fan-out and cannot detect head-of-line blocking")
	}
}

func TestHub_SlowSSEClientDoesNotDelayWebSockets(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:          bus,
		SendTimeout:  3 * time.Second,
		PingInterval: time.Minute,
	})
	ts := newHubTestServer(t, hub)

	// An SSE client that never reads its body: the same stall, on the other
	// transport, blocking the same shared goroutine.
	dialSSE(t, ts, "/sse?subscribe=*")

	fast := dialWS(t, ts, "/ws")
	fast.subscribe("probe.*")
	probes := drainText(fast)

	floodEvents(t, bus, "flood.event", 256)

	start := time.Now()
	for range 3 {
		publish(t, bus, "probe.tick", "probe/1")
	}
	for i := range 3 {
		select {
		case <-probes:
		case <-time.After(1500 * time.Millisecond):
			t.Fatalf("a WebSocket client got only %d of 3 events in %v: a stalled SSE client "+
				"blocks the shared fan-out goroutine, and WebSocket delivery with it",
				i, time.Since(start))
		}
	}
	if s := hub.Stats(); s.Kicked == 0 {
		t.Error("the SSE client was never kicked, so it was never saturated: this test " +
			"measured an unobstructed fan-out and cannot detect head-of-line blocking")
	}
}

// TestHub_SendTimeoutIsIgnored pins the deprecation: a 30s SendTimeout must not
// postpone the kick by so much as a second.
func TestHub_SendTimeoutIsIgnored(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:          bus,
		SendBuffer:   1,
		SendTimeout:  30 * time.Second,
		PingInterval: time.Minute,
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	floodEvents(t, bus, "flood.event", 64)

	// Stay unread for a moment: reading would drain the socket, free the
	// buffer and remove the very condition being measured.
	time.Sleep(300 * time.Millisecond)

	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatal("no kick within 2s: SendTimeout is still gating the kick, so the fan-out " +
			"is waiting out a configured 30s on a client that is already hopeless")
	}
	if code != 1013 {
		t.Errorf("close code: want 1013, got %d", code)
	}
}

// TestHub_SlowClientIsCountedOnce guards Stats().Kicked against the new failure
// mode dropping the wait creates: a kicked client stays registered until its
// pumps exit, so every event fanned out in the meantime reaches the same full
// buffer, and counting per drop would report the event rate rather than the
// number of clients lost.
func TestHub_SlowClientIsCountedOnce(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, SendBuffer: 1, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*") // and never reads again

	floodEvents(t, bus, "flood.event", 64)
	time.Sleep(300 * time.Millisecond) // let the whole flood fan out

	if s := hub.Stats(); s.Kicked != 1 {
		t.Errorf("kick count for one slow client: want 1, got %d", s.Kicked)
	}
}

// TestHub_BurstWithinSendBufferIsNotKicked is the over-reach guard. Kicking on
// a full buffer with no grace period is only defensible if the buffer is what
// decides — a burst that fits must still be delivered whole.
func TestHub_BurstWithinSendBufferIsNotKicked(t *testing.T) {
	t.Parallel()
	const burst = 50 // comfortably under the default SendBuffer of 64
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	for range burst {
		publish(t, bus, "thing.created", "thing/1")
	}

	for i := range burst {
		if _, ok := c.recvTimeout(2 * time.Second); !ok {
			t.Fatalf("a burst of %d fitting inside the send buffer lost the client after %d events",
				burst, i)
		}
	}
	if s := hub.Stats(); s.Kicked != 0 {
		t.Errorf("kicked a client over a burst that fits in its buffer: want 0 kicks, got %d", s.Kicked)
	}
	if s := hub.Stats(); s.Connections != 1 {
		t.Errorf("connections: want 1, got %d", s.Connections)
	}
}
