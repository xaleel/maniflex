package realtime

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xaleel/maniflex/events"
)

// VisibilityFunc controls per-event, per-client delivery.
// Return (false, _) to suppress the event for this client.
// Return (true, non-nil) to deliver a transformed copy instead of the original.
// Return (true, nil) to deliver the event unchanged.
type VisibilityFunc func(p *Principal, e events.Event) (deliver bool, transformed *events.Event)

// HubConfig configures a Hub.
type HubConfig struct {
	Bus            events.Bus    // required
	Authenticator  Authenticator // nil → AnonymousOnly{}
	Visibility     VisibilityFunc
	AllowPatterns  []string      // optional whitelist; empty = allow all
	Logger         *slog.Logger  // nil → slog.Default()
	PingInterval   time.Duration // 0 → 30s; SSE keepalive comment cadence
	SendBuffer     int           // 0 → 64; per-client outbound queue depth
	SendTimeout    time.Duration // 0 → 5s; slow-client kick threshold
	MaxMessageSize int64         // 0 → 64 KiB; inbound frame size limit
	Origins        []string      // allowed Origins for WS upgrade; empty = allow all

	// ResumeStore buffers recently fanned-out events so a reconnecting client
	// can replay what it missed (SSE Last-Event-ID, WS subscribe "after").
	// nil → resume disabled (hot path unchanged). See ResumeBuffer for the
	// zero-config shortcut.
	ResumeStore ResumeStore
	// ResumeBuffer is a shortcut: when ResumeStore is nil and ResumeBuffer > 0,
	// NewHub installs NewMemoryResumeStore(ResumeBuffer).
	ResumeBuffer int
}

// HubStats is a snapshot of hub metrics.
type HubStats struct {
	Connections int
	Kicked      int64
}

// Hub is a WebSocket + SSE event hub backed by an events.Bus.
// Mount Handler() at your WebSocket path and SSEHandler() at your SSE path.
// Zero registration on maniflex.Config — add it only when you need real-time.
type Hub struct {
	cfg     HubConfig
	clients sync.Map     // *hubClient → struct{} and *sseClient → struct{}
	connN   atomic.Int64 // active connections
	kickN   atomic.Int64 // slow-consumer kicks
	cancel  events.Cancel
	closed  atomic.Bool
	wg      sync.WaitGroup // tracks WS read+write goroutines only
	resume  ResumeStore    // nil when resume is disabled
}

// NewHub creates a Hub and subscribes it to the bus.
// Returns an error if Bus is nil or the bus subscription fails.
func NewHub(cfg HubConfig) (*Hub, error) {
	if cfg.Bus == nil {
		return nil, fmt.Errorf("realtime: HubConfig.Bus is required")
	}
	if cfg.Authenticator == nil {
		cfg.Authenticator = AnonymousOnly{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SendBuffer <= 0 {
		cfg.SendBuffer = 64
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 5 * time.Second
	}
	if cfg.MaxMessageSize <= 0 {
		cfg.MaxMessageSize = 64 * 1024
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.ResumeStore == nil && cfg.ResumeBuffer > 0 {
		cfg.ResumeStore = NewMemoryResumeStore(cfg.ResumeBuffer)
	}

	h := &Hub{cfg: cfg, resume: cfg.ResumeStore}

	cancel, err := cfg.Bus.Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: 1,
		Handler: func(_ context.Context, e events.Event) error {
			h.fanout(e)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: bus subscribe: %w", err)
	}
	h.cancel = cancel
	return h, nil
}

// Stats returns a snapshot of connection and kick counters.
func (h *Hub) Stats() HubStats {
	return HubStats{
		Connections: int(h.connN.Load()),
		Kicked:      h.kickN.Load(),
	}
}

// Shutdown closes all client connections, cancels the bus subscription, and
// waits for all WS goroutines to exit (or ctx to expire).
// Safe to call multiple times.
func (h *Hub) Shutdown(ctx context.Context) error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	h.cancel()

	// Signal all clients to close.
	h.clients.Range(func(k, _ any) bool {
		switch c := k.(type) {
		case *hubClient:
			c.sendClose(wsClose1001)
		case *sseClient:
			c.shutdown()
		}
		return true
	})

	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler returns the http.HandlerFunc that upgrades connections to WebSocket.
func (h *Hub) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.closed.Load() {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}

		// Origin check.
		if len(h.cfg.Origins) > 0 {
			if !originAllowed(r.Header.Get("Origin"), h.cfg.Origins) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}

		// Authenticate before upgrading.
		principal, err := h.cfg.Authenticator.Authenticate(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		key := r.Header.Get("Sec-Websocket-Key")
		if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "bad request: not a websocket upgrade", http.StatusBadRequest)
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			http.Error(w, "hijack failed", http.StatusInternalServerError)
			return
		}

		// Send 101 Switching Protocols.
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
		if _, err := io.WriteString(brw, resp); err != nil {
			conn.Close()
			return
		}
		if err := brw.Flush(); err != nil {
			conn.Close()
			return
		}

		c := &hubClient{
			hub:       h,
			conn:      conn,
			br:        brw.Reader,
			principal: principal,
			out:       make(chan []byte, h.cfg.SendBuffer),
			done:      make(chan struct{}),
			subs:      make(map[string][]string),
		}
		h.clients.Store(c, struct{}{})
		h.connN.Add(1)

		h.wg.Add(2)
		go c.readLoop()
		go c.writeLoop()
	}
}

// SSEHandler returns the http.HandlerFunc that serves a Server-Sent Events stream.
func (h *Hub) SSEHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.closed.Load() {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}

		// Authenticate.
		principal, err := h.cfg.Authenticator.Authenticate(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Parse subscribe patterns from query string.
		patterns := r.URL.Query()["subscribe"]
		if len(patterns) == 0 {
			patterns = []string{"*"}
		}

		// Validate against AllowPatterns; for SSE, any forbidden pattern → 403.
		if len(h.cfg.AllowPatterns) > 0 {
			for _, p := range patterns {
				if !patternAllowed(p, h.cfg.AllowPatterns) {
					http.Error(w, "forbidden pattern", http.StatusForbidden)
					return
				}
			}
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sc := &sseClient{
			hub:        h,
			w:          w,
			flusher:    flusher,
			principal:  principal,
			patterns:   patterns,
			out:        make(chan []byte, h.cfg.SendBuffer),
			shutdownCh: make(chan struct{}),
		}
		h.clients.Store(sc, struct{}{})
		h.connN.Add(1)
		// Defer the cleanup so a panic inside sc.run doesn't leak the entry
		// in h.clients or leave h.connN over-counted (which would block
		// Shutdown indefinitely as it waits for connN to reach 0).
		defer func() {
			h.clients.Delete(sc)
			h.connN.Add(-1)
		}()

		// Resume: replay events the client missed before entering the live
		// loop. Registration above already queues live events into sc.out, so
		// replay (written directly here) precedes them and the seam is at
		// worst at-least-once — clients drop anything ≤ their last cursor.
		h.replaySSE(w, flusher, sc, r)

		sc.run(r.Context()) // blocks until disconnect or shutdown
	}
}

// fanout delivers e to every connected client that has a matching subscription.
// When resume is enabled the event is first appended to the store, and the
// cursor it mints is stamped on every delivery so clients can resume from it.
func (h *Hub) fanout(e events.Event) {
	cursor := ""
	if h.resume != nil {
		cursor = h.resume.Append(e)
	}
	h.clients.Range(func(k, _ any) bool {
		switch c := k.(type) {
		case *hubClient:
			h.deliverWS(c, e, cursor)
		case *sseClient:
			h.deliverSSE(c, e, cursor)
		}
		return true
	})
}

func (h *Hub) deliverWS(c *hubClient, e events.Event, cursor string) {
	c.subMu.RLock()
	var matchingSubs []string
	for subID, patterns := range c.subs {
		if matchesAny(patterns, e.Type) {
			matchingSubs = append(matchingSubs, subID)
		}
	}
	c.subMu.RUnlock()

	for _, subID := range matchingSubs {
		ev, deliver := h.applyVisibility(c.principal, e)
		if !deliver {
			continue
		}
		if !h.enqueueWS(c, subID, cursor, ev) {
			return // client was kicked
		}
	}
}

// enqueueWS marshals one event message and queues it on the client's outbound
// channel, kicking the client (close 1013) if it can't keep up. Returns false
// when the client was kicked. Shared by live fan-out and resume replay.
func (h *Hub) enqueueWS(c *hubClient, subID, cursor string, ev events.Event) bool {
	payload := map[string]any{"op": "event", "subId": subID, "data": ev}
	if cursor != "" {
		payload["cursor"] = cursor
	}
	msg, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	frame := wsEncodeFrame(wsText, msg)
	select {
	case c.out <- frame:
		return true
	case <-time.After(h.cfg.SendTimeout):
		h.kickN.Add(1)
		c.sendClose(wsClose1013)
		return false
	}
}

func (h *Hub) deliverSSE(sc *sseClient, e events.Event, cursor string) {
	if !matchesAny(sc.patterns, e.Type) {
		return
	}
	ev, deliver := h.applyVisibility(sc.principal, e)
	if !deliver {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// Mirror the WS slow-consumer policy (roadmap §11B.8 / checkpoint H8):
	// after SendTimeout the slow client is kicked so its EventSource
	// reconnects, rather than silently dropping events into /dev/null.
	// kickN is shared with WS so operators see one number.
	select {
	case sc.out <- encodeSSEEvent("", cursor, data):
	case <-time.After(h.cfg.SendTimeout):
		h.kickN.Add(1)
		sc.shutdown()
	}
}

// replaySSE writes the backlog of events the SSE client missed since its
// Last-Event-ID, oldest-first, before the live loop starts. A no-op when resume
// is disabled or the client presents no cursor. If the cursor is too old (or
// from a previous hub epoch) it emits a single `resync` event so the client
// refetches state instead of silently skipping the gap.
func (h *Hub) replaySSE(w http.ResponseWriter, flusher http.Flusher, sc *sseClient, r *http.Request) {
	if h.resume == nil {
		return
	}
	last := lastEventID(r)
	if last == "" {
		return
	}
	evs, ok := h.resume.Replay(last)
	if !ok {
		io.WriteString(w, "event: resync\ndata: {}\n\n") //nolint:errcheck
		flusher.Flush()
		return
	}
	for _, be := range evs {
		if !matchesAny(sc.patterns, be.Event.Type) {
			continue
		}
		ev, deliver := h.applyVisibility(sc.principal, be.Event)
		if !deliver {
			continue
		}
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		w.Write(encodeSSEEvent("", be.Cursor, data)) //nolint:errcheck
	}
	flusher.Flush()
}

// lastEventID returns the resume cursor a reconnecting SSE client presents,
// preferring the standard Last-Event-ID header (sent automatically by
// EventSource) and falling back to the ?lastEventId= query parameter.
func lastEventID(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	return r.URL.Query().Get("lastEventId")
}

func (h *Hub) applyVisibility(p *Principal, e events.Event) (events.Event, bool) {
	if h.cfg.Visibility == nil {
		return e, true
	}
	deliver, transformed := h.cfg.Visibility(p, e)
	if !deliver {
		return events.Event{}, false
	}
	if transformed != nil {
		return *transformed, true
	}
	return e, true
}

// ── WebSocket client ──────────────────────────────────────────────────────────

type hubClient struct {
	hub        *Hub
	conn       net.Conn
	br         *bufio.Reader
	principal  *Principal
	out        chan []byte       // encoded WS frames; never closed by sender
	done       chan struct{}     // closed by sendClose to wake the write loop
	writeMu    sync.Mutex       // serialises writes to conn
	closeOnce  sync.Once        // ensures sendClose runs once
	removeOnce sync.Once        // ensures remove runs once
	subs       map[string][]string // subID → []pattern
	subMu      sync.RWMutex
	subSeq     atomic.Uint64
}

// sendClose writes a close frame with the given status code, closes the
// connection, and signals the write loop to exit. Safe to call concurrently.
func (c *hubClient) sendClose(code uint16) {
	c.closeOnce.Do(func() {
		payload := []byte{byte(code >> 8), byte(code)}
		frame := wsEncodeFrame(wsClose, payload)
		c.writeMu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		c.conn.Write(frame) //nolint:errcheck
		c.writeMu.Unlock()
		c.conn.Close()
		close(c.done)
	})
}

func (c *hubClient) remove() {
	c.removeOnce.Do(func() {
		c.hub.clients.Delete(c)
		c.hub.connN.Add(-1)
	})
}

// writeRaw writes a pre-encoded frame directly to conn under writeMu.
func (c *hubClient) writeRaw(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(frame)
	return err
}

// sendJSON enqueues a JSON message frame. Non-blocking: drops if channel full.
func (c *hubClient) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	frame := wsEncodeFrame(wsText, data)
	select {
	case c.out <- frame:
	default:
	}
}

func (c *hubClient) readLoop() {
	defer func() {
		c.remove()
		c.hub.wg.Done()
	}()
	for {
		f, err := c.recvFrame()
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				c.sendClose(wsClose1009)
			}
			return
		}
		switch f.opcode {
		case wsClose:
			c.sendClose(wsClose1000)
			return
		case wsPing:
			c.writeRaw(wsEncodeFrame(wsPong, f.payload)) //nolint:errcheck
		case wsText, wsBinary:
			c.handleMessage(f.payload)
		}
	}
}

func (c *hubClient) writeLoop() {
	defer func() {
		c.remove()
		c.hub.wg.Done()
	}()
	// Server-initiated heartbeat: send a WS ping every PingInterval so idle
	// connections survive L7 idle timeouts symmetrically with the SSE keepalive
	// comment. Compliant clients answer with a pong; the read loop drops it.
	ping := time.NewTicker(c.hub.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case frame := <-c.out:
			if err := c.writeRaw(frame); err != nil {
				return
			}
		case <-ping.C:
			if err := c.writeRaw(wsEncodeFrame(wsPing, nil)); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *hubClient) handleMessage(payload []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		c.sendJSON(map[string]any{"op": "error", "code": "BAD_MESSAGE", "msg": "invalid JSON"})
		return
	}
	var op string
	json.Unmarshal(raw["op"], &op) //nolint:errcheck

	switch op {
	case "subscribe":
		c.handleSubscribe(raw)
	case "unsubscribe":
		c.handleUnsubscribe(raw)
	case "ping":
		c.sendJSON(map[string]any{"op": "pong"})
	default:
		c.sendJSON(map[string]any{"op": "error", "code": "UNKNOWN_OP", "msg": "unknown op: " + op})
	}
}

func (c *hubClient) handleSubscribe(raw map[string]json.RawMessage) {
	var patterns []string
	json.Unmarshal(raw["patterns"], &patterns) //nolint:errcheck

	if len(patterns) == 0 {
		c.sendJSON(map[string]any{"op": "error", "code": "NO_PATTERNS", "msg": "patterns required"})
		return
	}

	allowed := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if len(c.hub.cfg.AllowPatterns) > 0 && !patternAllowed(p, c.hub.cfg.AllowPatterns) {
			c.sendJSON(map[string]any{
				"op":   "error",
				"code": "FORBIDDEN_PATTERN",
				"msg":  "pattern not allowed: " + p,
			})
		} else {
			allowed = append(allowed, p)
		}
	}

	if len(allowed) == 0 {
		return
	}

	subID := fmt.Sprintf("s_%d", c.subSeq.Add(1))
	c.subMu.Lock()
	c.subs[subID] = allowed
	c.subMu.Unlock()

	c.sendJSON(map[string]any{"op": "ack", "subId": subID})

	// Resume: an optional "after" cursor replays the events this subscription
	// missed. Replayed events carry their cursor so the client advances its
	// position; a too-old cursor yields a single "resync" control message.
	var after string
	json.Unmarshal(raw["after"], &after) //nolint:errcheck
	if after != "" && c.hub.resume != nil {
		c.replayWS(subID, allowed, after)
	}
}

// replayWS re-emits the events a resuming WS subscription missed, applying the
// same pattern match + visibility filter as live delivery.
func (c *hubClient) replayWS(subID string, patterns []string, after string) {
	evs, ok := c.hub.resume.Replay(after)
	if !ok {
		c.sendJSON(map[string]any{"op": "resync", "subId": subID})
		return
	}
	for _, be := range evs {
		if !matchesAny(patterns, be.Event.Type) {
			continue
		}
		ev, deliver := c.hub.applyVisibility(c.principal, be.Event)
		if !deliver {
			continue
		}
		if !c.hub.enqueueWS(c, subID, be.Cursor, ev) {
			return // client was kicked
		}
	}
}

func (c *hubClient) handleUnsubscribe(raw map[string]json.RawMessage) {
	var subID string
	json.Unmarshal(raw["subId"], &subID) //nolint:errcheck
	c.subMu.Lock()
	delete(c.subs, subID)
	c.subMu.Unlock()
	c.sendJSON(map[string]any{"op": "unsubscribed", "subId": subID})
}

// recvFrame reads one WebSocket frame (RFC 6455) from the client.
// Clients always mask their frames; server frames are unmasked.
type wsRawFrame struct {
	opcode  byte
	payload []byte
}

func (c *hubClient) recvFrame() (wsRawFrame, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.br, hdr); err != nil {
		return wsRawFrame{}, err
	}
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	plen := int(hdr[1] & 0x7f)

	switch plen {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return wsRawFrame{}, err
		}
		plen = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return wsRawFrame{}, err
		}
		plen = 0
		for _, b := range ext {
			plen = plen<<8 | int(b)
		}
	}

	// Reject oversized frames BEFORE allocating. Pre-fix a malicious client
	// could advertise a 2 GiB payload and force the server to allocate even
	// though the frame is malformed — roadmap §11B.7 / checkpoint H7.
	if plen < 0 || int64(plen) > c.hub.cfg.MaxMessageSize {
		return wsRawFrame{}, errFrameTooLarge
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return wsRawFrame{}, err
		}
	}

	data := make([]byte, plen)
	if _, err := io.ReadFull(c.br, data); err != nil {
		return wsRawFrame{}, err
	}
	if masked {
		for i, b := range data {
			data[i] = b ^ maskKey[i%4]
		}
	}

	return wsRawFrame{opcode: opcode, payload: data}, nil
}

// ── SSE client ────────────────────────────────────────────────────────────────

type sseClient struct {
	hub        *Hub
	w          http.ResponseWriter
	flusher    http.Flusher
	principal  *Principal
	patterns   []string
	out        chan []byte
	shutdownCh chan struct{}
	shutOnce   sync.Once
}

func (sc *sseClient) shutdown() {
	sc.shutOnce.Do(func() { close(sc.shutdownCh) })
}

func (sc *sseClient) run(ctx context.Context) {
	// Roadmap §11B.9 / checkpoint H9: emit an SSE comment line on a ticker
	// so idle connections aren't dropped by ALBs / NGINX (typical 30-60s
	// timeouts). The comment is invisible to EventSource consumers and
	// costs ~12 bytes per tick.
	keepalive := time.NewTicker(sc.hub.cfg.PingInterval)
	defer keepalive.Stop()

	for {
		select {
		case frame := <-sc.out:
			sc.w.Write(frame) //nolint:errcheck
			sc.flusher.Flush()
		case <-keepalive.C:
			io.WriteString(sc.w, ": keepalive\n\n") //nolint:errcheck
			sc.flusher.Flush()
		case <-ctx.Done():
			return
		case <-sc.shutdownCh:
			return
		}
	}
}

// encodeSSEEvent renders one Server-Sent Events frame. The optional event type
// and id lines are omitted when empty; data is the JSON payload bytes.
func encodeSSEEvent(eventType, id string, data []byte) []byte {
	var b strings.Builder
	if eventType != "" {
		b.WriteString("event: ")
		b.WriteString(eventType)
		b.WriteByte('\n')
	}
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return []byte(b.String())
}

// ── WebSocket frame encoding (server → client, no masking) ───────────────────

const (
	wsText    byte = 0x1
	wsBinary  byte = 0x2
	wsClose   byte = 0x8
	wsPing    byte = 0x9
	wsPong    byte = 0xA
	wsClose1000 uint16 = 1000
	wsClose1001 uint16 = 1001
	wsClose1009 uint16 = 1009 // Message Too Big
	wsClose1013 uint16 = 1013
)

// errFrameTooLarge is returned by recvFrame when an inbound frame's advertised
// payload exceeds HubConfig.MaxMessageSize. The read loop converts this to a
// 1009 close so a malicious client can't force the server to allocate a
// multi-gigabyte buffer just to discover the frame is malformed.
var errFrameTooLarge = errors.New("ws: frame exceeds MaxMessageSize")

// wsEncodeFrame encodes one server-to-client WebSocket frame (FIN=1, no mask).
func wsEncodeFrame(opcode byte, payload []byte) []byte {
	n := len(payload)
	var hdr []byte
	switch {
	case n <= 125:
		hdr = []byte{0x80 | opcode, byte(n)}
	case n <= 65535:
		hdr = []byte{0x80 | opcode, 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0] = 0x80 | opcode
		hdr[1] = 127
		for i := 0; i < 8; i++ {
			hdr[9-i] = byte(n >> (uint(i) * 8))
		}
	}
	return append(hdr, payload...)
}

// wsAcceptKey computes the Sec-WebSocket-Accept value per RFC 6455 §1.3.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ── Pattern and origin helpers ────────────────────────────────────────────────

// matchesAny reports whether eventType matches any pattern in the list.
// Uses path.Match glob syntax: "invoice.*", "*.created", "*".
func matchesAny(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, _ := path.Match(p, eventType); ok {
			return true
		}
	}
	return false
}

// patternAllowed reports whether pattern is permitted by the allowList.
func patternAllowed(pattern string, allowList []string) bool {
	for _, a := range allowList {
		if a == pattern {
			return true
		}
	}
	return false
}

// originAllowed reports whether the request Origin header value is in the list.
func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if a == origin {
			return true
		}
	}
	return false
}
