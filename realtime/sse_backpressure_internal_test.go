package realtime

// White-box test for RT-7: a non-reading SSE client must not pin its handler
// goroutine (and, through it, Shutdown) forever. It is internal because it drives
// sseWriteTimeout down to a test-friendly value — the write deadline is enforced
// by the real net.Conn, so the client has to be a genuine non-reading TCP peer,
// not a fake ResponseWriter.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
)

const sseRunFrame = "realtime.(*sseClient).run"

func countRunning(fn string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), fn)
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitRunning(fn string, want int, d time.Duration) (int, bool) {
	deadline := time.Now().Add(d)
	for {
		got := countRunning(fn)
		if got == want {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialNonReadingSSE opens an SSE stream, reads the 200 response head, then stops
// reading — the client whose full receive buffer makes the server's write block.
func dialNonReadingSSE(t *testing.T, ts *httptest.Server) net.Conn {
	t.Helper()
	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	fmt.Fprintf(conn, "GET /sse?subscribe=* HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n\r\n", host)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read SSE response head: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: want 200, got %d", resp.StatusCode)
	}
	// Deliberately do not read resp.Body: the receive buffer fills and the
	// server's writes block, which is the whole point.
	return conn
}

// floodBlocking publishes enough large events that the hub's writer fills the
// non-reading client's socket buffer and blocks in w.Write.
func floodBlocking(t *testing.T, bus events.Publisher) {
	t.Helper()
	blob := strings.Repeat("x", 16*1024)
	data, _ := json.Marshal(map[string]string{"blob": blob})
	for range 256 {
		if err := bus.Publish(context.Background(), events.Event{
			ID: "flood", Source: "/t", Type: "flood.event", Subject: "x/1", Data: data,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
}

func newInternalHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	bus := inproc.New()
	// Long ping interval so the keepalive is not what triggers the blocking
	// write — the flood is.
	hub, err := NewHub(HubConfig{Bus: bus, PingInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", hub.SSEHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return hub, ts
}

// TestSSE_WriteDeadlineUnblocksStuckHandler covers the leak: a non-reading
// client's handler must exit on its own once a write exceeds the deadline,
// rather than blocking on w.Write for the life of the process.
func TestSSE_WriteDeadlineUnblocksStuckHandler(t *testing.T) {
	old := sseWriteTimeout
	sseWriteTimeout = 150 * time.Millisecond
	defer func() { sseWriteTimeout = old }()

	hub, ts := newInternalHub(t)
	base := countRunning(sseRunFrame)

	dialNonReadingSSE(t, ts)
	if n, ok := waitRunning(sseRunFrame, base+1, 2*time.Second); !ok {
		t.Fatalf("SSE handler never started: want %d run goroutines, got %d", base+1, n)
	}

	floodBlocking(t, hub.cfg.Bus)

	// The stuck write exceeds the 150ms deadline, errors, and sc.run returns.
	if n, ok := waitRunning(sseRunFrame, base, 3*time.Second); !ok {
		t.Errorf("a non-reading SSE client's handler never exited: want %d run goroutines, got %d "+
			"— the write blocked past the deadline with no bound", base, n)
	}
}

// TestSSE_ShutdownDrainsStuckHandler covers the second half: Shutdown must wait
// for the SSE handler goroutine (not only the WS pumps), and the write deadline
// is what keeps that wait bounded when the client has stopped reading.
func TestSSE_ShutdownDrainsStuckHandler(t *testing.T) {
	old := sseWriteTimeout
	sseWriteTimeout = time.Second // long enough that a non-waiting Shutdown returns first
	defer func() { sseWriteTimeout = old }()

	hub, ts := newInternalHub(t)
	base := countRunning(sseRunFrame)

	dialNonReadingSSE(t, ts)
	if n, ok := waitRunning(sseRunFrame, base+1, 2*time.Second); !ok {
		t.Fatalf("SSE handler never started: want %d run goroutines, got %d", base+1, n)
	}

	floodBlocking(t, hub.cfg.Bus)
	time.Sleep(200 * time.Millisecond) // let the writer block in w.Write

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown did not complete: %v (the stuck SSE write was never bounded)", err)
	}

	// Shutdown returned nil, so it waited for the handler; the goroutine must be
	// gone the instant it returns, with no further poll.
	if n := countRunning(sseRunFrame); n != base {
		t.Errorf("Shutdown returned before the SSE handler drained: want %d run goroutines, got %d",
			base, n)
	}
}
