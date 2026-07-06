package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
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

// captureLogs installs a slog default that records emitted records and returns
// the slice plus a restore func. Not parallel-safe (mutates the global default).
func captureLogs(t *testing.T) *[]capturedLog {
	t.Helper()
	var mu sync.Mutex
	logs := &[]capturedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &mu, logs: logs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func countLevel(logs []capturedLog, lvl slog.Level) int {
	n := 0
	for _, l := range logs {
		if l.level == lvl {
			n++
		}
	}
	return n
}

type recordingPublisher struct {
	mu        sync.Mutex
	published []Event
}

func (p *recordingPublisher) Publish(_ context.Context, e Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, e)
	return nil
}

func (p *recordingPublisher) PublishBatch(_ context.Context, es []Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, es...)
	return nil
}

func (p *recordingPublisher) Close() error { return nil }

// A handler that always fails with MaxRetry=2 (3 attempts) and no DLQ must emit
// one WARN per retryable attempt (2) and one ERROR on exhaustion, so a
// persistently-failing handler is never silent.
func TestDeliverWithRetry_AlwaysFails_NoDLQ_LogsRetriesAndDrop(t *testing.T) {
	logs := captureLogs(t)
	calls := 0
	sub := Subscription{
		Handler:  func(context.Context, Event) error { calls++; return errors.New("boom") },
		MaxRetry: 2, // Backoff nil → no sleeping
	}

	DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "e1", Type: "widget.created"})

	if calls != 3 {
		t.Fatalf("expected 3 handler attempts, got %d", calls)
	}
	if got := countLevel(*logs, slog.LevelWarn); got != 2 {
		t.Errorf("expected 2 retry WARNs, got %d (%v)", got, *logs)
	}
	if got := countLevel(*logs, slog.LevelError); got != 1 {
		t.Errorf("expected 1 exhaustion ERROR, got %d (%v)", got, *logs)
	}
}

// With a DLQ configured, exhaustion must still ERROR (not silently ack away) and
// the event must be re-published under the DLQ type.
func TestDeliverWithRetry_AlwaysFails_WithDLQ_LogsAndDeadLetters(t *testing.T) {
	logs := captureLogs(t)
	pub := &recordingPublisher{}
	sub := Subscription{
		Handler:  func(context.Context, Event) error { return errors.New("boom") },
		MaxRetry: 1, // 2 attempts, 1 retryable
		DLQ:      "widget.created.dlq",
	}

	DeliverWithRetry(context.Background(), pub, sub, Event{ID: "e2", Type: "widget.created"})

	if got := countLevel(*logs, slog.LevelWarn); got != 1 {
		t.Errorf("expected 1 retry WARN, got %d (%v)", got, *logs)
	}
	if got := countLevel(*logs, slog.LevelError); got != 1 {
		t.Errorf("expected 1 exhaustion ERROR, got %d (%v)", got, *logs)
	}
	if len(pub.published) != 1 || pub.published[0].Type != "widget.created.dlq" {
		t.Fatalf("expected 1 DLQ publish of type widget.created.dlq, got %+v", pub.published)
	}
}

// A handler that succeeds on the first attempt must not log anything.
func TestDeliverWithRetry_Succeeds_NoLogs(t *testing.T) {
	logs := captureLogs(t)
	sub := Subscription{
		Handler:  func(context.Context, Event) error { return nil },
		MaxRetry: 3,
	}

	DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "e3", Type: "widget.created"})

	if len(*logs) != 0 {
		t.Errorf("expected no logs on success, got %v", *logs)
	}
}
