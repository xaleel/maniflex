package kafka

// Audit EV-13: a failed FetchMessage retried after a bare
// time.Sleep(time.Second) — fixed interval, no jitter, blind to the context —
// and the loop logged nothing at all, so a consumer that could not reach the
// broker was indistinguishable from one with nothing to consume.
//
// Unlike the Redis adapter, this one has no seam: kafkago.Reader is a concrete
// type constructed inside Subscribe, so runConsumer cannot be exercised without
// a broker. What is reachable is the reporting half, which is where the
// behaviour choice lives — WARN per retry, ERROR exactly once when a run of
// failures stops looking transient.
//
//	go test ./events/kafka/... -run TestReadBackoff

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
)

type capturedLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type captureHandler struct {
	mu   *sync.Mutex
	logs *[]capturedLog
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	*h.logs = append(*h.logs, capturedLog{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func captureLogs(t *testing.T) *[]capturedLog {
	t.Helper()
	var mu sync.Mutex
	logs := &[]capturedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &mu, logs: logs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// Driving the reporting with a real ReadBackoff rather than hand-picked
// arguments: the escalate-once contract spans both, and asserting them
// separately would let a mismatch through.
func TestReadBackoff_ReportsWarnPerRetryAndErrorOnceAtCap(t *testing.T) {
	logs := captureLogs(t)

	bo := events.ReadBackoff{Min: 10 * time.Millisecond, Max: 40 * time.Millisecond}
	for range 8 {
		attempt, delay, escalate := bo.Next()
		logReadFailure("app.invoice.created", attempt, delay, escalate, errors.New("connection refused"))
	}

	var warns, errs int
	for _, l := range *logs {
		switch l.level {
		case slog.LevelWarn:
			warns++
		case slog.LevelError:
			errs++
		}
	}

	if errs != 1 {
		t.Errorf("%d ERRORs across 8 failures, want exactly 1: a long outage should not bury the logs", errs)
	}
	if warns != 7 {
		t.Errorf("%d WARNs, want 7 (every retry that is not the escalation)", warns)
	}
	if len(*logs) != 8 {
		t.Errorf("%d records for 8 failed reads: a silent retry is the bug this fixes", len(*logs))
	}
}

// The attributes are what makes the log actionable: without topic and error
// an operator cannot tell which consumer is stuck or why.
func TestReadBackoff_LogCarriesDiagnosticAttributes(t *testing.T) {
	logs := captureLogs(t)

	logReadFailure("app.invoice.created", 3, 400*time.Millisecond, false, errors.New("connection refused"))

	if len(*logs) != 1 {
		t.Fatalf("got %d records, want 1", len(*logs))
	}
	got := (*logs)[0]

	if got.attrs["topic"] != "app.invoice.created" {
		t.Errorf("topic = %v; an operator cannot tell which consumer is stuck", got.attrs["topic"])
	}
	// slog.Int stores an int64, so compare against one: an untyped 3 is an int
	// and would never match through the interface.
	if got.attrs["attempt"] != int64(3) {
		t.Errorf("attempt = %#v, want int64(3)", got.attrs["attempt"])
	}
	if got.attrs["retry_in"] != 400*time.Millisecond {
		t.Errorf("retry_in = %v, want 400ms", got.attrs["retry_in"])
	}
	if got.attrs["error"] != "connection refused" {
		t.Errorf("error = %v; the cause is missing", got.attrs["error"])
	}
}

// A recovered-then-failing-again consumer deserves a fresh ERROR, because the
// second outage is a new event an operator needs to see.
func TestReadBackoff_EscalatesAgainAfterRecovery(t *testing.T) {
	logs := captureLogs(t)

	bo := events.ReadBackoff{Min: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	report := func() {
		attempt, delay, escalate := bo.Next()
		logReadFailure("t", attempt, delay, escalate, errors.New("boom"))
	}

	for range 4 {
		report()
	}
	bo.Reset() // a successful read
	for range 4 {
		report()
	}

	var errs int
	for _, l := range *logs {
		if l.level == slog.LevelError {
			errs++
		}
	}
	if errs != 2 {
		t.Errorf("%d ERRORs across two separate outages, want 2", errs)
	}
}
