package e2e

// The server the framework starts had no timeouts of any kind. A client that
// opens a connection and dribbles its headers — never sending the blank line that
// ends them — was allowed to hold it open indefinitely; enough of those exhaust
// the file descriptors and no request reaches the pipeline at all. Config now
// carries a ReadHeaderTimeout (10s by default) and net/http hangs up (DX-1).
//
// httptest.Server builds its own http.Server, so this has to drive a real one:
// maniflex.Start on a real port, and a raw TCP connection as the client.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// startRealServer boots a maniflex server on a free port and returns its address.
func startRealServer(t *testing.T, cfg maniflex.Config) string {
	t.Helper()

	// Take a port, then hand it straight back — a race in principle, fine in a test.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	cfg.Port = port
	cfg.PathPrefix = "/api"
	cfg.DisableAutoMigrate = true
	cfg.ShutdownTimeout = 2 * time.Second

	srv := maniflex.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never came up on %s", addr)
	return ""
}

func TestSlowloris_HeaderDribbleIsCutOff(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t, maniflex.Config{ReadHeaderTimeout: 300 * time.Millisecond})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Open a request and never finish the header block: no blank line, ever.
	if _, err := fmt.Fprint(conn, "GET /api/health HTTP/1.1\r\nHost: localhost\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Generously past ReadHeaderTimeout. The server must have hung up (EOF, reset,
	// or a 408 it writes before closing) well before this fires.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	start := time.Now()
	_, readErr := conn.Read(make([]byte, 256))
	elapsed := time.Since(start)

	if errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatal("the connection was still open 3s into a half-sent header block — the server has no ReadHeaderTimeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("server took %v to cut off a dribbling client, want ~300ms", elapsed)
	}
}

// The deadline applies to headers, not to the connection's whole life: an
// ordinary request still gets served.
func TestSlowloris_NormalRequestUnaffected(t *testing.T) {
	t.Parallel()
	addr := startRealServer(t, maniflex.Config{ReadHeaderTimeout: 300 * time.Millisecond})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprint(conn, "GET /api/health HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); len(got) < 12 || got[:12] != "HTTP/1.1 200" {
		t.Errorf("response: got %q, want a 200", got)
	}
}
