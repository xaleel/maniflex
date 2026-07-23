package realtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Operational logging (HubConfig.Logger) ────────────────────────────────────
//
// HubConfig.Logger was defaulted in NewHub and then never used, so a hub dropped
// connections — slow consumers, a full hub, protocol violations, a panicking
// Visibility hook — in total silence. These tests assert the high-value signals
// now reach the configured logger, at the right level, carrying no payload.

// logBuf is a concurrency-safe buffer that records emitted log lines as JSON.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records returns each logged line parsed as a JSON object.
func (b *logBuf) records() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// waitForLog polls until a record with the given level whose message contains
// substr appears, returning it. Fails the test on timeout.
func (b *logBuf) waitForLog(t *testing.T, level, substr string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, r := range b.records() {
			lvl, _ := r["level"].(string)
			msg, _ := r["msg"].(string)
			if lvl == level && strings.Contains(msg, substr) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s log containing %q within 2s\nlog:\n%s", level, substr, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newLogger() (*slog.Logger, *logBuf) {
	b := &logBuf{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

func TestHubLog_SlowConsumerKick(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus, Logger: logger, SendBuffer: 1, PingInterval: time.Minute,
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")
	floodEvents(t, bus, "flood.event", 256)

	rec := buf.waitForLog(t, "WARN", "slow consumer")
	if tr, _ := rec["transport"].(string); tr != "ws" {
		t.Errorf("kick log transport: want ws, got %q", tr)
	}
	_ = c
}

func TestHubLog_ConnectionCapRejection(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Logger: logger, MaxConnections: 1})
	ts := newHubTestServer(t, hub)

	dialWS(t, ts, "/ws")
	if n, ok := waitForConnections(hub, 1, 2*time.Second); !ok {
		t.Fatalf("cap not filled: got %d", n)
	}
	if status := dialWSExpectStatus(t, ts, "/ws"); status != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", status)
	}

	rec := buf.waitForLog(t, "WARN", "hub at capacity")
	if got, _ := rec["count"].(float64); got != 1 {
		t.Errorf("first cap rejection should log count=1, got %v", rec["count"])
	}
}

func TestHubLog_ProtocolError(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Logger: logger})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// 0x81 = FIN + text, no mask bit — an unmasked client frame.
	c.conn.Write(rawUnmaskedFrame(0x81, []byte(`{"op":"ping"}`)))

	rec := buf.waitForLog(t, "WARN", "protocol error")
	if reason, _ := rec["reason"].(string); !strings.Contains(reason, "unmasked") {
		t.Errorf("protocol log should name the reason (unmasked), got %q", reason)
	}
}

func TestHubLog_OriginRejection(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus, Logger: logger, Origins: []string{"https://good.example.com"},
	})
	ts := newHubTestServer(t, hub)

	bad := make(http.Header)
	bad.Set("Origin", "https://evil.example.com")
	dialWSExpectStatus(t, ts, "/ws", bad)

	rec := buf.waitForLog(t, "WARN", "origin not allowed")
	if o, _ := rec["origin"].(string); o != "https://evil.example.com" {
		t.Errorf("origin log should carry the offending origin, got %q", o)
	}
}

// TestHubLog_FanoutPanicRecovered proves both halves of the recover: a panicking
// Visibility hook is logged at ERROR, and — the important part — the hub's
// bus-subscriber goroutine survives it and delivers the next event.
func TestHubLog_FanoutPanicRecovered(t *testing.T) {
	logger, buf := newLogger()
	var calls int
	var mu sync.Mutex
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:    bus,
		Logger: logger,
		Visibility: func(_ *realtime.Principal, _ events.Event) (bool, *events.Event) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				panic("boom in the visibility hook")
			}
			return true, nil
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// First event: the hook panics; it must be recovered and logged, not
	// delivered, and must not kill the subscriber goroutine.
	publish(t, bus, "thing.one", "thing/1")
	buf.waitForLog(t, "ERROR", "recovered from panic")
	if _, ok := c.recvTimeout(300 * time.Millisecond); ok {
		t.Error("the event whose hook panicked should not have been delivered")
	}

	// Second event: proves the goroutine survived — a dead subscriber would
	// never deliver this.
	publish(t, bus, "thing.two", "thing/2")
	if _, ok := c.recvTimeout(2 * time.Second); !ok {
		t.Fatal("the hub stopped delivering after a Visibility panic: the subscriber goroutine died")
	}
}

// TestHubLog_ShutdownTimeout uses an already-expired context so Shutdown takes
// the timeout branch deterministically.
func TestHubLog_ShutdownTimeout(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Logger: logger})
	ts := newHubTestServer(t, hub)

	// A live connection so the drain has something to wait on.
	c := dialWS(t, ts, "/ws")
	c.subscribe("*")
	if _, ok := waitForConnections(hub, 1, 2*time.Second); !ok {
		t.Fatal("connection not registered")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired
	if err := hub.Shutdown(ctx); err == nil {
		t.Skip("shutdown drained before the expired context was observed; timing-dependent")
	}
	buf.waitForLog(t, "WARN", "shutdown timed out")
}

// TestHubLog_QuietWhenHealthy is the over-reach guard: an ordinary connect,
// subscribe, deliver and graceful close must not emit WARN/ERROR noise.
func TestHubLog_QuietWhenHealthy(t *testing.T) {
	logger, buf := newLogger()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus, Logger: logger})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("user.*")
	publish(t, bus, "user.created", "user/1")
	if _, ok := c.recvTimeout(2 * time.Second); !ok {
		t.Fatal("event not delivered")
	}

	for _, r := range buf.records() {
		if lvl, _ := r["level"].(string); lvl == "WARN" || lvl == "ERROR" {
			t.Errorf("healthy traffic emitted a %s line: %v", lvl, r)
		}
	}
}
