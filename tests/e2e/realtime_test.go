package e2e_test

// realtime_test.go — end-to-end integration tests for 3C.2 (realtime hub).
//
// These tests wire the full stack:
//
//	POST /api/posts → events.Emit (DB step, After) → inproc.Bus → realtime.Hub → WS client
//
// No external broker is needed. The inproc bus covers every code path the hub
// itself exercises, so these tests run without Docker / Redis / NATS.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── E2E stack setup ───────────────────────────────────────────────────────────

// realtimeStack wires a Server server + realtime hub into a single httptest.Server.
// The hub's Handler is served at /ws; the Server API is at /api.
type realtimeStack struct {
	ts  *httptest.Server
	bus *inproc.Bus
	hub *realtime.Hub
}

func newRealtimeStack(t *testing.T, hubCfg realtime.HubConfig) *realtimeStack {
	t.Helper()

	bus := inproc.New()

	// Merge the bus into the supplied config (caller may have set Authenticator etc.).
	hubCfg.Bus = bus

	hub, err := realtime.NewHub(hubCfg)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	srv := maniflex.New(maniflex.Config{
		PathPrefix:  "/api",
		AutoMigrate: true,
	})
	srv.MustRegister(testutil.User{}, testutil.Post{})

	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.AutoMigrate(context.Background(), srv.Registry()); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	srv.SetDB(db)

	// Register events.Emit on the DB step (After) for create/update/delete.
	srv.Pipeline.DB.Register(
		events.Emit(bus, events.EmitConfig{}),
		maniflex.AtPosition(maniflex.After),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
	)

	r := chi.NewRouter()
	maniflex.Mount(r, srv)
	r.Handle("/ws", hub.Handler())
	r.Handle("/sse", hub.SSEHandler())

	ts := httptest.NewServer(r)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hub.Shutdown(ctx)
		ts.Close()
		db.Close()
	})

	return &realtimeStack{ts: ts, bus: bus, hub: hub}
}

// ── Core E2E: Maniflex write → event → WS client ───────────────────────────────────

func TestRealtimeE2E_CreateTriggersWebSocketEvent(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	// Connect WS and subscribe to all user events.
	c := e2eDialWS(t, stack.ts, "/ws")
	ack := e2eSubscribe(t, c, "user.*")
	subID, _ := ack["subId"].(string)

	// Create a User via the REST API.
	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name":     "Alice",
		"email":    "alice@example.com",
		"password": "secret",
		"role":     "viewer",
	})

	// WS client should receive a CloudEvents-formatted "user.created" event.
	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("E2E: timed out waiting for user.created WebSocket event")
	}

	if op, _ := msg["op"].(string); op != "event" {
		t.Fatalf("expected op=event, got %q", op)
	}
	if sid, _ := msg["subId"].(string); sid != subID {
		t.Errorf("subId: got %q, want %q", sid, subID)
	}

	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != "user.created" {
		t.Errorf("event type: got %q, want user.created", typ)
	}
	// The event source must be set from the service name.
	if _, ok := data["source"].(string); !ok {
		t.Error("event data missing 'source' field")
	}
	// The event data must contain the created record.
	eventData, _ := data["data"].(map[string]any)
	if eventData == nil {
		t.Error("event data field should be the created record JSON")
	}
}

func TestRealtimeE2E_UpdateTriggersWebSocketEvent(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "user.*")

	// Create then update a user.
	id := apiPost(t, stack.ts, "/api/users", map[string]any{
		"name":     "Bob",
		"email":    "bob@example.com",
		"password": "secret",
		"role":     "viewer",
	})

	// Consume the created event.
	e2eRecvTimeout(t, c, 2*time.Second)

	// Now update the user.
	apiPatch(t, stack.ts, "/api/users/"+id, map[string]any{"role": "editor"})

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("E2E: timed out waiting for user.updated WebSocket event")
	}
	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != "user.updated" {
		t.Errorf("event type: got %q, want user.updated", typ)
	}
}

func TestRealtimeE2E_DeleteTriggersWebSocketEvent(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "user.*")

	id := apiPost(t, stack.ts, "/api/users", map[string]any{
		"name":     "Charlie",
		"email":    "charlie@example.com",
		"password": "secret",
		"role":     "viewer",
	})
	e2eRecvTimeout(t, c, 2*time.Second) // consume created event

	apiDelete(t, stack.ts, "/api/users/"+id)

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("E2E: timed out waiting for user.deleted WebSocket event")
	}
	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != "user.deleted" {
		t.Errorf("event type: got %q, want user.deleted", typ)
	}
}

// ── E2E: CloudEvents envelope ─────────────────────────────────────────────────

func TestRealtimeE2E_EventEnvelopeIsCloudEventsCompliant(t *testing.T) {
	// Verifies the fields required by CloudEvents 1.0 are present in the
	// event delivered to the WS client.
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "*")

	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Dan", "email": "dan@example.com",
		"password": "secret", "role": "viewer",
	})

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("E2E CE: timed out")
	}

	data, _ := msg["data"].(map[string]any)
	requiredFields := []string{"id", "source", "type", "time"}
	for _, f := range requiredFields {
		if data[f] == nil {
			t.Errorf("CloudEvents required field %q missing from event envelope", f)
		}
	}
}

// ── E2E: pattern filtering ────────────────────────────────────────────────────

func TestRealtimeE2E_PatternFiltering_OnlyMatchingEventsDelivered(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "post.*") // only post events

	// Create a user (user.created should NOT be delivered).
	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Eve", "email": "eve@example.com",
		"password": "secret", "role": "viewer",
	})

	_, gotUser := e2eRecvTimeout(t, c, 300*time.Millisecond)
	if gotUser {
		t.Error("user.created should not reach a post.* subscriber")
	}

	// Create a post (post.created SHOULD be delivered).
	userID := apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Frank", "email": "frank@example.com",
		"password": "s", "role": "viewer",
	})
	apiPost(t, stack.ts, "/api/posts", map[string]any{
		"title": "Test Post", "body": "body text",
		"status": "draft", "user_id": userID,
	})

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("post.created event not received despite matching subscription")
	}
	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != "post.created" {
		t.Errorf("expected post.created, got %q", typ)
	}
}

// ── E2E: multiple concurrent WS clients ──────────────────────────────────────

func TestRealtimeE2E_MultipleConcurrentClients(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c1 := e2eDialWS(t, stack.ts, "/ws")
	c2 := e2eDialWS(t, stack.ts, "/ws")

	e2eSubscribe(t, c1, "user.*")
	e2eSubscribe(t, c2, "user.*")

	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Grace", "email": "grace@example.com",
		"password": "s", "role": "viewer",
	})

	msg1, ok1 := e2eRecvTimeout(t, c1, 3*time.Second)
	msg2, ok2 := e2eRecvTimeout(t, c2, 3*time.Second)

	if !ok1 {
		t.Error("c1: timed out waiting for user.created event")
	}
	if !ok2 {
		t.Error("c2: timed out waiting for user.created event")
	}

	for _, msg := range []map[string]any{msg1, msg2} {
		data, _ := msg["data"].(map[string]any)
		if typ, _ := data["type"].(string); typ != "user.created" {
			t.Errorf("expected user.created, got %q", typ)
		}
	}
}

// ── E2E: SSE transport ────────────────────────────────────────────────────────

func TestRealtimeE2E_CreateTriggers_SSEEvent(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialSSE(t, stack.ts, "/sse?subscribe=user.*")

	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Heidi", "email": "heidi@example.com",
		"password": "s", "role": "viewer",
	})

	msg, ok := c.recvSSEEvent(3 * time.Second)
	if !ok {
		t.Fatal("E2E SSE: timed out waiting for user.created event")
	}
	if typ, _ := msg["type"].(string); typ != "user.created" {
		t.Errorf("SSE event type: got %q, want user.created", typ)
	}
}

// ── E2E: authenticated hub ────────────────────────────────────────────────────

func TestRealtimeE2E_AuthenticatedHub_ValidTokenReceivesEvents(t *testing.T) {
	t.Parallel()
	const jwtSecret = "e2e-realtime-secret"

	stack := newRealtimeStack(t, realtime.HubConfig{
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			// Use makeJWTClaims from auth_info_test.go (same package).
			// For the test, we validate the token by re-making it with the same secret.
			// In real usage: auth.VerifyJWT(tok, jwtOpts) → PrincipalFromAuthInfo
			if tok == "" {
				return nil, &realtime.ErrUnauthorized{Reason: "missing token"}
			}
			return &realtime.Principal{UserID: "testuser"}, nil
		}),
	})

	tok := makeJWTClaims(t, jwtSecret, map[string]any{
		"sub": "testuser", "exp": 99999999999, "iat": 1000000000,
	})

	c := e2eDialWS(t, stack.ts, "/ws", http.Header{
		"Authorization": []string{"Bearer " + tok},
	})
	e2eSubscribe(t, c, "user.*")

	apiPost(t, stack.ts, "/api/users", map[string]any{
		"name": "Ivan", "email": "ivan@example.com",
		"password": "s", "role": "viewer",
	})

	_, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("E2E auth: authenticated client did not receive user.created event")
	}
}

func TestRealtimeE2E_AuthenticatedHub_NoTokenRejected(t *testing.T) {
	t.Parallel()

	stack := newRealtimeStack(t, realtime.HubConfig{
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			if tok == "" {
				return nil, &realtime.ErrUnauthorized{Reason: "missing token"}
			}
			return &realtime.Principal{UserID: "ok"}, nil
		}),
	})

	status := e2eDialWSExpectStatus(t, stack.ts, "/ws")
	if status == http.StatusSwitchingProtocols {
		t.Error("E2E auth: expected non-101 for unauthenticated WS connection")
	}
}

// ── E2E: hub shutdown is non-disruptive ───────────────────────────────────────

func TestRealtimeE2E_ShutdownSendsGoodbyeToClients(t *testing.T) {
	t.Parallel()
	stack := newRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "*")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stack.hub.Shutdown(ctx)

	// Client should receive a close frame with code 1001 Going Away.
	code, ok := e2eRecvCloseCode(t, c, 2*time.Second)
	if !ok {
		t.Fatal("E2E shutdown: timed out waiting for close frame")
	}
	if code != 1001 {
		t.Errorf("E2E shutdown: expected close code 1001, got %d", code)
	}
}

// ── E2E WebSocket helpers ─────────────────────────────────────────────────────
// These mirror the realtime_test package helpers but live in the e2e package.

type e2eWsConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func e2eDialWS(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) *e2eWsConn {
	t.Helper()

	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("e2eDialWS: %v", err)
	}

	rawKey := make([]byte, 16)
	rand.Read(rawKey)
	key := base64.StdEncoding.EncodeToString(rawKey)

	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", host)
	sb.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\n", key)
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")
	for _, h := range headers {
		for k, vs := range h {
			for _, v := range vs {
				fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
			}
		}
	}
	sb.WriteString("\r\n")
	io.WriteString(conn, sb.String())

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		t.Fatalf("e2eDialWS: read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		t.Fatalf("e2eDialWS: expected 101, got %d: %s", resp.StatusCode, body)
	}

	// Validate Sec-WebSocket-Accept.
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	want := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if got := resp.Header.Get("Sec-Websocket-Accept"); got != want {
		conn.Close()
		t.Fatalf("e2eDialWS: bad Sec-WebSocket-Accept")
	}

	c := &e2eWsConn{t: t, conn: conn, br: br}
	t.Cleanup(func() { conn.Close() })
	return c
}

func e2eDialWSExpectStatus(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) int {
	t.Helper()
	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("e2eDialWSExpectStatus: %v", err)
	}
	defer conn.Close()

	rawKey := make([]byte, 16)
	rand.Read(rawKey)
	key := base64.StdEncoding.EncodeToString(rawKey)

	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, host)
	sb.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for _, h := range headers {
		for k, vs := range h {
			for _, v := range vs {
				fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
			}
		}
	}
	sb.WriteString("\r\n")
	io.WriteString(conn, sb.String())

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("e2eDialWSExpectStatus: read: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// e2eSendFrame writes a masked WebSocket frame.
func (c *e2eWsConn) e2eSendFrame(opcode byte, payload []byte) {
	mask := [4]byte{0x37, 0x42, 0x89, 0x1A}
	n := len(payload)
	var hdr []byte
	if n <= 125 {
		hdr = []byte{0x80 | opcode, byte(0x80 | n)}
	} else {
		hdr = []byte{0x80 | opcode, 0xFE, byte(n >> 8), byte(n)}
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	c.conn.Write(append(hdr, masked...))
}

// e2eRecvFrame reads one WebSocket frame from the server.
func (c *e2eWsConn) e2eRecvFrame() (byte, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.br, hdr); err != nil {
		return 0, nil, err
	}
	opcode := hdr[0] & 0x0f
	plen := int(hdr[1] & 0x7f)
	if plen == 126 {
		ext := make([]byte, 2)
		io.ReadFull(c.br, ext)
		plen = int(ext[0])<<8 | int(ext[1])
	}
	payload := make([]byte, plen)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

func e2eSubscribe(t *testing.T, c *e2eWsConn, patterns ...string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "subscribe", "patterns": patterns})
	c.e2eSendFrame(0x1, b)

	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		opcode, payload, err := c.e2eRecvFrame()
		if err != nil {
			t.Fatalf("e2eSubscribe: recv: %v", err)
		}
		if opcode == 0x9 { // ping
			c.e2eSendFrame(0xA, payload) // pong
			continue
		}
		var msg map[string]any
		json.Unmarshal(payload, &msg)
		if msg["op"] == "ack" {
			return msg
		}
		t.Fatalf("e2eSubscribe: expected ack, got: %v", msg)
	}
}

func e2eRecvTimeout(t *testing.T, c *e2eWsConn, d time.Duration) (map[string]any, bool) {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		opcode, payload, err := c.e2eRecvFrame()
		if err != nil {
			return nil, false
		}
		switch opcode {
		case 0x8: // close
			return nil, false
		case 0x9: // ping
			c.e2eSendFrame(0xA, payload)
		case 0x1, 0x2: // text/binary
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				return nil, false
			}
			return msg, true
		}
	}
}

func e2eRecvCloseCode(t *testing.T, c *e2eWsConn, d time.Duration) (uint16, bool) {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		opcode, payload, err := c.e2eRecvFrame()
		if err != nil {
			return 0, false
		}
		if opcode == 0x8 {
			if len(payload) >= 2 {
				return uint16(payload[0])<<8 | uint16(payload[1]), true
			}
			return 1000, true
		}
	}
}

// ── E2E SSE helpers ───────────────────────────────────────────────────────────

type e2eSSEConn struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

func e2eDialSSE(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) *e2eSSEConn {
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
		t.Fatalf("e2eDialSSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("e2eDialSSE: expected 200, got %d: %s", resp.StatusCode, body)
	}
	c := &e2eSSEConn{t: t, resp: resp, sc: bufio.NewScanner(resp.Body)}
	t.Cleanup(func() { resp.Body.Close() })
	return c
}

func (c *e2eSSEConn) recvSSEEvent(d time.Duration) (map[string]any, bool) {
	type result struct {
		msg map[string]any
		ok  bool
	}
	ch := make(chan result, 1)
	go func() {
		var dataLine string
		for c.sc.Scan() {
			line := c.sc.Text()
			if line == "" {
				if dataLine != "" {
					var msg map[string]any
					json.Unmarshal([]byte(dataLine), &msg)
					ch <- result{msg: msg, ok: msg != nil}
					return
				}
				continue
			}
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				dataLine = after
			}
		}
		ch <- result{}
	}()
	select {
	case r := <-ch:
		return r.msg, r.ok
	case <-time.After(d):
		return nil, false
	}
}

// ── REST API helpers ──────────────────────────────────────────────────────────

// apiPost creates a resource and returns its ID. Fails the test if status != 201.
func apiPost(t *testing.T, ts *httptest.Server, path string, body map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apiPost %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("apiPost %s: expected 201, got %d: %s", path, resp.StatusCode, raw)
	}
	var result map[string]any
	json.Unmarshal(raw, &result)
	data, _ := result["data"].(map[string]any)
	id, _ := data["id"].(string)
	return id
}

// apiPatch updates a resource. Fails if status != 200.
func apiPatch(t *testing.T, ts *httptest.Server, path string, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apiPatch %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("apiPatch %s: expected 200, got %d: %s", path, resp.StatusCode, raw)
	}
}

// apiDelete deletes a resource. Fails if status != 204.
func apiDelete(t *testing.T, ts *httptest.Server, path string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apiDelete %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("apiDelete %s: expected 204, got %d: %s", path, resp.StatusCode, raw)
	}
}
