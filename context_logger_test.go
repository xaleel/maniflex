package maniflex

// ctx.Logger() memoises its logger on first call. The context outlives the request
// goroutine — a ctx.GoBackground closure captures it and logs from another
// goroutine while the request is still in later steps (the framework's own
// file-cleanup middleware does exactly this) — so the unsynchronised lazy write
// raced those reads (PERF-3).
//
// The race itself is only *proven* by `go test -race`; these pin the property that
// makes it impossible: the logger is built exactly once, so no two goroutines can
// be writing it. That is observable without -race — a racing build hands different
// callers different loggers.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func loggerCtx() *ServerContext {
	return &ServerContext{
		logger:      slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		serviceName: "billing",
		RequestID:   "req-42",
		TraceID:     "trace-7",
	}
}

// Every concurrent caller must get the same logger. Without exactly-once, several
// goroutines each see the empty field, build their own, and overwrite each other —
// so the callers that raced walk away with different instances.
func TestLogger_ConcurrentCallersShareOneLogger(t *testing.T) {
	t.Parallel()

	ctx := loggerCtx()
	const n = 64
	got := make([]*slog.Logger, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			got[i] = ctx.Logger()
		})
	}
	close(start) // release them together, onto the very first call
	wg.Wait()

	for i, l := range got {
		if l == nil {
			t.Fatalf("goroutine %d got a nil logger", i)
		}
		if l != got[0] {
			t.Fatalf("goroutine %d got a different *slog.Logger than goroutine 0 — the "+
				"memoised logger was built more than once, so concurrent callers were "+
				"writing the field while others read it", i)
		}
	}
}

// The memoisation itself must survive: repeat calls hand back the same logger
// rather than paying a fresh base.With(...) per emission.
func TestLogger_MemoisedAcrossCalls(t *testing.T) {
	t.Parallel()

	ctx := loggerCtx()
	first := ctx.Logger()
	if second := ctx.Logger(); second != first {
		t.Error("Logger() rebuilt on a second call — the memoisation is what keeps it off the hot path")
	}
}

// A background goroutine logging after the request goroutine has moved on is the
// scenario the race came from; it must still get a usable, attributed logger.
func TestLogger_UsableFromABackgroundGoroutine(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var lines []string
	h := &captureHandler{mu: &mu, lines: &lines}
	ctx := &ServerContext{logger: slog.New(h), serviceName: "billing", RequestID: "req-42"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx.Logger().Info("from the background")
	}()
	ctx.Logger().Info("from the request")
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d: %v", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "billing") || !strings.Contains(l, "req-42") {
			t.Errorf("log line lost its request attributes: %q", l)
		}
	}
}

// captureHandler records each message plus its attrs as a flat string.
type captureHandler struct {
	mu    *sync.Mutex
	lines *[]string
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.lines = append(*h.lines, b.String())
	return nil
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &captureHandler{mu: h.mu, lines: h.lines, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }
