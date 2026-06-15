package realtime_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── memoryResumeStore unit tests (black-box via the exported API) ─────────────

func ev(typ string) events.Event {
	return events.Event{ID: "id-" + typ, Type: typ, Data: json.RawMessage(`{}`)}
}

func TestMemoryResumeStore_AppendReplay(t *testing.T) {
	t.Parallel()
	s := realtime.NewMemoryResumeStore(10)
	c1 := s.Append(ev("a.1"))
	c2 := s.Append(ev("a.2"))
	c3 := s.Append(ev("a.3"))

	evs, ok := s.Replay(c1)
	if !ok {
		t.Fatal("Replay(c1) ok=false, want true")
	}
	if len(evs) != 2 {
		t.Fatalf("Replay(c1) returned %d events, want 2", len(evs))
	}
	if evs[0].Event.Type != "a.2" || evs[0].Cursor != c2 {
		t.Errorf("evs[0]=%q/%q, want a.2/%s", evs[0].Event.Type, evs[0].Cursor, c2)
	}
	if evs[1].Event.Type != "a.3" || evs[1].Cursor != c3 {
		t.Errorf("evs[1]=%q/%q, want a.3/%s", evs[1].Event.Type, evs[1].Cursor, c3)
	}

	// Resuming from the newest cursor yields nothing, but is still "ok".
	evs, ok = s.Replay(c3)
	if !ok || len(evs) != 0 {
		t.Errorf("Replay(c3)=%d events ok=%v, want 0/true", len(evs), ok)
	}
}

func TestMemoryResumeStore_UnknownEpochAndMalformed(t *testing.T) {
	t.Parallel()
	s := realtime.NewMemoryResumeStore(10)
	s.Append(ev("a.1"))

	if _, ok := s.Replay("deadbeefdeadbeef:1"); ok {
		t.Error("Replay with foreign epoch should be ok=false")
	}
	if _, ok := s.Replay("not-a-cursor"); ok {
		t.Error("Replay with malformed cursor should be ok=false")
	}
}

func TestMemoryResumeStore_EvictionSignalsGap(t *testing.T) {
	t.Parallel()
	s := realtime.NewMemoryResumeStore(2)
	c1 := s.Append(ev("a.1"))
	c2 := s.Append(ev("a.2"))
	s.Append(ev("a.3"))
	s.Append(ev("a.4")) // window now {a.3, a.4}; a.2 (c2's next) is the oldest

	// c1's next event (a.2) was evicted → a real gap → caller must resync.
	if _, ok := s.Replay(c1); ok {
		t.Error("Replay(c1) after eviction should be ok=false (gap)")
	}
	// c2's next event (a.3) is still retained → replays cleanly.
	evs, ok := s.Replay(c2)
	if !ok || len(evs) != 2 || evs[0].Event.Type != "a.3" || evs[1].Event.Type != "a.4" {
		t.Errorf("Replay(c2)=%v ok=%v, want [a.3 a.4]/true", evs, ok)
	}
}

// ── SSE resume (Last-Event-ID) ────────────────────────────────────────────────

func TestSSE_ResumeReplaysMissedEvents(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 100})
	ts := newHubTestServer(t, hub)

	a := dialSSE(t, ts, "/sse?subscribe=*")
	publish(t, bus, "order.created", "o/1")
	m1, ok := a.recvFull(2 * time.Second)
	if !ok || m1.id == "" {
		t.Fatalf("first event missing id (resume cursor): %+v", m1)
	}

	// Published while the resuming client is absent.
	publish(t, bus, "order.updated", "o/2")
	publish(t, bus, "order.deleted", "o/3")

	b := dialSSE(t, ts, "/sse?subscribe=*", lastEventIDHeader(m1.id))
	got := []string{}
	for i := 0; i < 2; i++ {
		m, ok := b.recvFull(2 * time.Second)
		if !ok {
			t.Fatalf("resume: timed out waiting for replayed event %d", i)
		}
		got = append(got, eventType(m.data))
	}
	if got[0] != "order.updated" || got[1] != "order.deleted" {
		t.Errorf("replayed %v, want [order.updated order.deleted]", got)
	}
}

func TestSSE_ResumeTooOldEmitsResync(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 2})
	ts := newHubTestServer(t, hub)

	a := dialSSE(t, ts, "/sse?subscribe=*")
	publish(t, bus, "order.1", "o/1")
	m1, ok := a.recvFull(2 * time.Second)
	if !ok || m1.id == "" {
		t.Fatalf("first event missing id: %+v", m1)
	}
	// Overflow the 2-slot buffer so m1.id is evicted.
	for _, s := range []string{"order.2", "order.3", "order.4"} {
		publish(t, bus, s, "o/x")
		a.recvFull(2 * time.Second)
	}

	b := dialSSE(t, ts, "/sse?subscribe=*", lastEventIDHeader(m1.id))
	m, ok := b.recvFull(2 * time.Second)
	if !ok || m.event != "resync" {
		t.Errorf("expected resync event, got event=%q ok=%v", m.event, ok)
	}
}

// ── WebSocket resume (subscribe "after") ──────────────────────────────────────

func TestWS_ResumeReplaysMissedEvents(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 100})
	ts := newHubTestServer(t, hub)

	a := dialWS(t, ts, "/ws")
	a.subscribe("*")
	publish(t, bus, "order.created", "o/1")
	m1 := a.recv()
	cursor1, _ := m1["cursor"].(string)
	if cursor1 == "" {
		t.Fatalf("first WS event missing cursor: %v", m1)
	}

	publish(t, bus, "order.updated", "o/2")
	publish(t, bus, "order.deleted", "o/3")

	b := dialWS(t, ts, "/ws")
	b.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"*"}, "after": cursor1})
	b.mustRecvOp("ack")

	got := []string{}
	for range 2 {
		msg := b.recv()
		if op, _ := msg["op"].(string); op != "event" {
			t.Fatalf("expected op=event, got %v", msg)
		}
		if _, ok := msg["cursor"].(string); !ok {
			t.Errorf("replayed WS event missing cursor: %v", msg)
		}
		data, _ := msg["data"].(map[string]any)
		got = append(got, eventType(data))
	}
	if got[0] != "order.updated" || got[1] != "order.deleted" {
		t.Errorf("replayed %v, want [order.updated order.deleted]", got)
	}
}

func TestWS_ResumeTooOldEmitsResync(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 2})
	ts := newHubTestServer(t, hub)

	a := dialWS(t, ts, "/ws")
	a.subscribe("*")
	publish(t, bus, "order.1", "o/1")
	cursor1, _ := a.recv()["cursor"].(string)
	if cursor1 == "" {
		t.Fatal("first WS event missing cursor")
	}
	for _, s := range []string{"order.2", "order.3", "order.4"} {
		publish(t, bus, s, "o/x")
		a.recv()
	}

	b := dialWS(t, ts, "/ws")
	b.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"*"}, "after": cursor1})
	b.mustRecvOp("ack")
	b.mustRecvOp("resync")
}

// ── WebSocket server-initiated heartbeat (10.9 / workstream B) ────────────────

func TestWS_ServerHeartbeatPing(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	for range 20 {
		f, err := c.recvFrame()
		if err != nil {
			t.Fatalf("reading frames: %v", err)
		}
		if f.opcode == wsOpcodePing {
			return // success: server initiated a ping
		}
	}
	t.Fatal("no server-initiated ping frame observed within deadline")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// sseMessage is a decoded SSE frame including its id and event lines, which the
// shared recvEvent helper discards.
type sseMessage struct {
	id    string
	event string
	data  map[string]any
}

func (c *sseClient) recvFull(d time.Duration) (sseMessage, bool) {
	type result struct {
		m  sseMessage
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		var msg sseMessage
		hasData := false
		for c.sc.Scan() {
			line := c.sc.Text()
			if line == "" {
				if hasData {
					ch <- result{m: msg, ok: true}
					return
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				msg.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				msg.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var parsed map[string]any
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &parsed) //nolint:errcheck
				msg.data = parsed
				hasData = true
			}
		}
		ch <- result{}
	}()
	select {
	case r := <-ch:
		return r.m, r.ok
	case <-time.After(d):
		return sseMessage{}, false
	}
}

func lastEventIDHeader(id string) http.Header {
	h := make(http.Header)
	h.Set("Last-Event-ID", id)
	return h
}

func eventType(data map[string]any) string {
	t, _ := data["type"].(string)
	return t
}
