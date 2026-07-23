package realtime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// Tests for the reachable coverage gaps identified by seam: extended-length
// frame encode/decode, the oversized-frame close, the WS bad-upgrade and SSE
// closed-hub rejections, the SSE keepalive tick, replay pattern-filtering, and
// the resume store's zero-capacity default.

// publishBlob publishes an event whose JSON payload carries an n-byte blob, so
// the encoded frame crosses the 125 / 65535 byte length-form boundaries.
func publishBlob(t *testing.T, bus events.Publisher, typ string, n int) string {
	t.Helper()
	blob := strings.Repeat("z", n)
	data, _ := json.Marshal(map[string]string{"blob": blob})
	if err := bus.Publish(context.Background(), events.Event{
		ID: "id", Source: "/t", Type: typ, Subject: "x/1", Data: data,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return blob
}

// ── Item 1: server encodes extended-length frames (wsEncodeFrame 126 + 127) ────

func TestWSFrame_ServerEncodesExtendedLength(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	for _, n := range []int{1000, 70000} { // 126-form, then 127-form
		blob := publishBlob(t, bus, "big.event", n)
		msg, ok := c.recvTimeout(3 * time.Second)
		if !ok {
			t.Fatalf("n=%d: no event (server failed to encode an extended-length frame)", n)
		}
		data, _ := msg["data"].(map[string]any)
		inner, _ := data["data"].(map[string]any)
		if got, _ := inner["blob"].(string); got != blob {
			t.Errorf("n=%d: payload round-trip lost data: got %d bytes, want %d", n, len(got), len(blob))
		}
	}
}

// ── Item 2: recvFrame parses extended-length inbound frames (126 + 127) ────────

func TestWSFrame_AcceptsInboundExtendedLength(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, MaxMessageSize: 1 << 20, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")

	for _, n := range []int{1000, 70000} { // 126-form, then 127-form
		c.sendJSON(map[string]any{"op": "ping", "pad": strings.Repeat("q", n)})
		c.mustRecvOp("pong")
	}
}

// ── Item 2b (same seam): an oversized frame is rejected with 1009 ──────────────

func TestWSFrame_OversizedDataFrameClosed1009(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: time.Minute}) // MaxMessageSize 64 KiB default
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")

	// A masked text frame whose 127-form length claims 100 MiB. recvFrame
	// checks the advertised length against MaxMessageSize before reading (or
	// even allocating) the payload, so the header alone triggers the close.
	hdr := []byte{0x81, 0x80 | 127, 0x00, 0x00, 0x00, 0x00, 0x06, 0x40, 0x00, 0x00}
	if _, err := c.conn.Write(hdr); err != nil {
		t.Fatalf("write frame header: %v", err)
	}
	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatal("no close frame for an oversized inbound frame")
	}
	if code != 1009 {
		t.Errorf("oversized frame close code: want 1009 (message too big), got %d", code)
	}
}

// ── Item 3a: a non-upgrade request to the WS endpoint gets 400 ─────────────────

func TestWS_NonUpgradeRequestReturns400(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	resp, err := http.Get(ts.URL + "/ws") // a plain GET, no Upgrade / Sec-WebSocket-Key
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("plain GET on the WS endpoint: want 400, got %d", resp.StatusCode)
	}
}

// ── Item 3b: the SSE endpoint refuses new connections after Shutdown (503) ─────

func TestSSE_AfterShutdownReturns503(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.Shutdown(ctx)

	if status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*"); status != http.StatusServiceUnavailable {
		t.Errorf("SSE after shutdown: want 503, got %d", status)
	}
}

// ── Item 4: an idle SSE stream emits keepalive comments ────────────────────────

func TestSSE_KeepaliveComment(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, PingInterval: 50 * time.Millisecond})
	ts := newHubTestServer(t, hub)

	r := openRawSSE(t, ts, "/sse?subscribe=*")
	if !r.waitLine(": keepalive", 2*time.Second) {
		t.Error("no keepalive comment on an idle SSE stream within 2s")
	}
}

// ── Item 5: replay skips events that don't match the resuming subscription ─────

func TestWS_ReplaySkipsNonMatchingPattern(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 64, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// Buffer three events; the middle one won't match the resuming pattern.
	publish(t, bus, "post.created", "post/1")
	first, _ := c.recvTimeout(3 * time.Second)
	cursor, _ := first["cursor"].(string)
	if cursor == "" {
		t.Fatal("no cursor on the first event")
	}
	publish(t, bus, "user.created", "user/1") // will be replayed and skipped
	c.recvTimeout(3 * time.Second)
	publish(t, bus, "post.created", "post/2") // will be replayed and delivered
	c.recvTimeout(3 * time.Second)

	// Resume from the first cursor with a pattern that matches only posts: the
	// replay window is [user.created, post.created], the user event is skipped.
	c.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"post.*"}, "after": cursor})
	c.mustRecvOp("ack")

	msg, ok := c.recvTimeout(3 * time.Second)
	if !ok {
		t.Fatal("no replayed event")
	}
	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != "post.created" {
		t.Errorf("replay delivered %q; the non-matching user.created should have been skipped", typ)
	}
}

func TestSSE_ReplaySkipsNonMatchingPattern(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, ResumeBuffer: 64, PingInterval: time.Minute})
	ts := newHubTestServer(t, hub)

	// A live subscriber captures the id (cursor) of the first buffered event.
	live := openRawSSE(t, ts, "/sse?subscribe=*")
	publish(t, bus, "post.created", "post/1")
	id, _, ok := live.next(3 * time.Second)
	if !ok || id == "" {
		t.Fatalf("no id on the first SSE event (ok=%v id=%q)", ok, id)
	}
	publish(t, bus, "user.created", "user/1") // replayed, skipped
	publish(t, bus, "post.created", "post/2") // replayed, delivered

	// Reconnect resuming from that id with a posts-only subscription.
	h := make(http.Header)
	h.Set("Last-Event-ID", id)
	r := openRawSSE(t, ts, "/sse?subscribe=post.*", h)
	_, data, ok := r.next(3 * time.Second)
	if !ok {
		t.Fatal("no replayed SSE event")
	}
	if typ, _ := data["type"].(string); typ != "post.created" {
		t.Errorf("SSE replay delivered %q; the non-matching user.created should have been skipped", typ)
	}
}

// ── Item 6: NewMemoryResumeStore falls back to a default capacity when ≤ 0 ─────

func TestResume_ZeroCapacityDefaults(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -5} {
		store := realtime.NewMemoryResumeStore(capacity)
		c1 := store.Append(events.Event{ID: "1", Type: "a.one"})
		store.Append(events.Event{ID: "2", Type: "a.two"})

		evs, ok := store.Replay(c1)
		if !ok {
			t.Errorf("capacity %d: replay reported a gap where none exists", capacity)
			continue
		}
		if len(evs) != 1 || evs[0].Event.ID != "2" {
			t.Errorf("capacity %d: replay after the first cursor: want [event 2], got %+v", capacity, evs)
		}
	}
}

// ── Raw SSE reader (captures id: lines and comment lines) ──────────────────────

type rawSSE struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

func openRawSSE(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) *rawSSE {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Accept", "text/event-stream")
	for _, h := range headers {
		for k, vs := range h {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("openRawSSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("openRawSSE: want 200, got %d", resp.StatusCode)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &rawSSE{t: t, resp: resp, sc: bufio.NewScanner(resp.Body)}
}

// next reads one SSE event, returning its id: and decoded data: within d.
func (r *rawSSE) next(d time.Duration) (id string, data map[string]any, ok bool) {
	type res struct {
		id   string
		data map[string]any
		ok   bool
	}
	ch := make(chan res, 1)
	go func() {
		var curID, dataLine string
		for r.sc.Scan() {
			line := r.sc.Text()
			if line == "" {
				if dataLine != "" {
					var m map[string]any
					json.Unmarshal([]byte(dataLine), &m) //nolint:errcheck
					ch <- res{curID, m, m != nil}
					return
				}
				continue
			}
			if after, ok := strings.CutPrefix(line, "id: "); ok {
				curID = after
			}
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				dataLine = after
			}
		}
		ch <- res{}
	}()
	select {
	case v := <-ch:
		return v.id, v.data, v.ok
	case <-time.After(d):
		return "", nil, false
	}
}

// waitLine reports whether a raw line containing substr arrives within d.
func (r *rawSSE) waitLine(substr string, d time.Duration) bool {
	ch := make(chan bool, 1)
	go func() {
		for r.sc.Scan() {
			if strings.Contains(r.sc.Text(), substr) {
				ch <- true
				return
			}
		}
		ch <- false
	}()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		return false
	}
}
