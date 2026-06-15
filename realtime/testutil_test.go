// Package realtime_test tests the realtime hub using a minimal stdlib-only
// WebSocket client. No external dependencies are needed for tests.
package realtime_test

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── WebSocket frame opcodes (RFC 6455) ────────────────────────────────────────

const (
	wsOpcodeText   byte = 0x1
	wsOpcodeBinary byte = 0x2
	wsOpcodeClose  byte = 0x8
	wsOpcodePing   byte = 0x9
	wsOpcodePong   byte = 0xA
)

// wsFrame holds a decoded frame from the server.
type wsFrame struct {
	opcode  byte
	payload []byte
}

// wsClient is a minimal RFC 6455 WebSocket client for tests.
// Frames from client → server are masked (required by spec).
// Frames from server → client are unmasked.
type wsClient struct {
	t      *testing.T
	conn   net.Conn
	br     *bufio.Reader
	mu     sync.Mutex
	closed bool
}

// dialWS opens a WebSocket connection to path on ts with optional extra headers.
// Fails the test immediately if the 101 upgrade is not received.
func dialWS(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) *wsClient {
	t.Helper()

	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dialWS: tcp dial: %v", err)
	}

	rawKey := make([]byte, 16)
	rand.Read(rawKey)
	key := base64.StdEncoding.EncodeToString(rawKey)

	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", host)
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
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

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		t.Fatalf("dialWS: write upgrade: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		t.Fatalf("dialWS: read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		t.Fatalf("dialWS: expected 101, got %d: %s", resp.StatusCode, body)
	}

	// Validate Sec-WebSocket-Accept per RFC 6455 §1.3.
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	wantAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if got := resp.Header.Get("Sec-Websocket-Accept"); got != wantAccept {
		conn.Close()
		t.Fatalf("dialWS: bad Sec-WebSocket-Accept: got %q, want %q", got, wantAccept)
	}

	c := &wsClient{t: t, conn: conn, br: br}
	t.Cleanup(func() { c.closeGracefully() })
	return c
}

// dialWSExpectStatus performs a WebSocket upgrade and returns the HTTP status
// without asserting 101. Useful for testing auth-rejection paths.
func dialWSExpectStatus(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) int {
	t.Helper()

	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dialWSExpectStatus: tcp dial: %v", err)
	}
	defer conn.Close()

	rawKey := make([]byte, 16)
	rand.Read(rawKey)
	key := base64.StdEncoding.EncodeToString(rawKey)

	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", host)
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
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
		t.Fatalf("dialWSExpectStatus: read response: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// ── Frame encoding / decoding ─────────────────────────────────────────────────

// sendFrame writes one WebSocket frame (client masking applied).
func (c *wsClient) sendFrame(opcode byte, payload []byte) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	maskKey := [4]byte{0x37, 0x42, 0x89, 0x1A}
	n := len(payload)

	var hdr []byte
	switch {
	case n <= 125:
		hdr = []byte{0x80 | opcode, byte(0x80 | n)}
	case n <= 65535:
		hdr = []byte{0x80 | opcode, 0xFE, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0] = 0x80 | opcode
		hdr[1] = 0xFF
		for i := 0; i < 8; i++ {
			hdr[9-i] = byte(n >> (uint(i) * 8))
		}
	}

	hdr = append(hdr, maskKey[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	c.conn.Write(append(hdr, masked...))
}

// recvFrame reads one WebSocket frame from the server (no masking).
func (c *wsClient) recvFrame() (wsFrame, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.br, hdr); err != nil {
		return wsFrame{}, err
	}

	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	plen := int(hdr[1] & 0x7f)

	switch plen {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return wsFrame{}, err
		}
		plen = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return wsFrame{}, err
		}
		plen = 0
		for _, b := range ext {
			plen = plen<<8 | int(b)
		}
	}

	var mkey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mkey[:]); err != nil {
			return wsFrame{}, err
		}
	}

	data := make([]byte, plen)
	if _, err := io.ReadFull(c.br, data); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i, b := range data {
			data[i] = b ^ mkey[i%4]
		}
	}

	return wsFrame{opcode: opcode, payload: data}, nil
}

// ── High-level message helpers ────────────────────────────────────────────────

// sendJSON encodes v as JSON and sends it as a text frame.
func (c *wsClient) sendJSON(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("wsClient.sendJSON: marshal: %v", err)
	}
	c.sendFrame(wsOpcodeText, b)
}

// sendPing sends a WebSocket ping frame.
func (c *wsClient) sendPing() { c.sendFrame(wsOpcodePing, []byte("ping")) }

// recv blocks until a text/binary JSON frame arrives; fails on close frame.
func (c *wsClient) recv() map[string]any {
	c.t.Helper()
	for {
		f, err := c.recvFrame()
		if err != nil {
			c.t.Fatalf("wsClient.recv: %v", err)
		}
		switch f.opcode {
		case wsOpcodeClose:
			c.t.Fatalf("wsClient.recv: unexpected close frame (payload: %x)", f.payload)
		case wsOpcodePing:
			c.sendFrame(wsOpcodePong, f.payload)
		case wsOpcodeText, wsOpcodeBinary:
			var msg map[string]any
			if err := json.Unmarshal(f.payload, &msg); err != nil {
				c.t.Fatalf("wsClient.recv: unmarshal: %v\npayload: %s", err, f.payload)
			}
			return msg
		}
	}
}

// recvTimeout blocks up to d for a text/binary frame.
// Returns (nil, false) on timeout or close.
func (c *wsClient) recvTimeout(d time.Duration) (map[string]any, bool) {
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		f, err := c.recvFrame()
		if err != nil {
			return nil, false
		}
		switch f.opcode {
		case wsOpcodeClose:
			return nil, false
		case wsOpcodePing:
			c.sendFrame(wsOpcodePong, f.payload)
		case wsOpcodeText, wsOpcodeBinary:
			var msg map[string]any
			if err := json.Unmarshal(f.payload, &msg); err != nil {
				return nil, false
			}
			return msg, true
		}
	}
}

// recvCloseCode blocks up to d for a close frame and returns its status code.
// Returns (0, false) on timeout.
func (c *wsClient) recvCloseCode(d time.Duration) (uint16, bool) {
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})

	for {
		f, err := c.recvFrame()
		if err != nil {
			return 0, false
		}
		if f.opcode == wsOpcodeClose {
			if len(f.payload) >= 2 {
				return uint16(f.payload[0])<<8 | uint16(f.payload[1]), true
			}
			return 1000, true
		}
	}
}

// mustRecvOp receives the next message and asserts its "op" field equals wantOp.
func (c *wsClient) mustRecvOp(wantOp string) map[string]any {
	c.t.Helper()
	msg := c.recv()
	if got, _ := msg["op"].(string); got != wantOp {
		c.t.Fatalf("mustRecvOp: got op=%q, want %q (msg: %v)", got, wantOp, msg)
	}
	return msg
}

// subscribe sends a subscribe message and returns the ack (asserts op=="ack").
func (c *wsClient) subscribe(patterns ...string) map[string]any {
	c.t.Helper()
	c.sendJSON(map[string]any{"op": "subscribe", "patterns": patterns})
	return c.mustRecvOp("ack")
}

// closeGracefully sends a close frame then closes the connection.
func (c *wsClient) closeGracefully() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	// Close frame: FIN + close opcode, masked, status 1000 normal.
	maskKey := [4]byte{0x00, 0x00, 0x00, 0x00}
	statusBytes := []byte{0x03, 0xE8} // 1000 big-endian
	masked := []byte{statusBytes[0] ^ maskKey[0], statusBytes[1] ^ maskKey[1]}
	frame := append([]byte{0x88, 0x82}, maskKey[:]...)
	frame = append(frame, masked...)
	c.conn.Write(frame)
	c.conn.Close()
}

// ── SSE client ────────────────────────────────────────────────────────────────

// sseClient reads Server-Sent Events from a hub SSE endpoint.
type sseClient struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

// dialSSE opens an SSE connection to path on ts. Subscribe patterns should be
// encoded into the path as ?subscribe=... query parameters by the caller.
func dialSSE(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) *sseClient {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("dialSSE: new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for _, h := range headers {
		for k, vs := range h {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}

	// No global timeout — individual recvEvent calls use deadlines.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("dialSSE: do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("dialSSE: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("dialSSE: expected Content-Type text/event-stream, got %q", ct)
	}

	c := &sseClient{t: t, resp: resp, sc: bufio.NewScanner(resp.Body)}
	t.Cleanup(func() { resp.Body.Close() })
	return c
}

// dialSSEExpectStatus connects and returns the HTTP status without asserting 200.
func dialSSEExpectStatus(t *testing.T, ts *httptest.Server, path string, headers ...http.Header) int {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dialSSEExpectStatus: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// recvEvent reads the next SSE event within d. Returns (nil, false) on timeout.
// The "data:" line is decoded as JSON into a map.
func (c *sseClient) recvEvent(d time.Duration) (map[string]any, bool) {
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

// ── JWT helper ────────────────────────────────────────────────────────────────

// makeTestJWT creates an HS256-signed JWT with the supplied claims.
// Mirrors makeJWTClaims in tests/e2e/auth_info_test.go.
func makeTestJWT(secret string, claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	cb, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(cb)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(hdr + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hdr + "." + payload + "." + sig
}

// bearerHeader returns an http.Header with the Authorization: Bearer <token> set.
func bearerHeader(token string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	return h
}

// newHubTestServer creates an httptest.Server that serves the hub's WebSocket handler
// at /ws and its SSE handler at /sse.
func newHubTestServer(t *testing.T, h interface {
	Handler() http.HandlerFunc
	SSEHandler() http.HandlerFunc
}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.Handler())
	mux.HandleFunc("/sse", h.SSEHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}
