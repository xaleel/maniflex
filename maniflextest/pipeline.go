package maniflextest

import (
	"context"
	"log/slog"
	"slices"
	"sync"
)

// The framework reports pipeline order by logging one DEBUG record per
// middleware. The message and attribute keys below are matched here so that
// consumers do not have to match them in their own tests: a log message is not
// API, and a test that greps for one breaks on a patch release that rewords it.
const (
	traceEnterMessage = "middleware enter"
	traceStepKey      = "step"
	traceNameKey      = "middleware"
)

// traceLevel is the level the framework logs pipeline steps at. The recorder
// reports itself enabled for it whatever the configured logger would do, so
// recording works against a logger left at the default level.
const traceLevel = slog.LevelDebug

// pipelineRecorder captures the framework's pipeline trace and forwards every
// record to the handler the consumer configured, so enabling recording does not
// cost them their own logging.
//
// Server.do drains it after each request, which is what scopes the report to one
// request. There is deliberately no correlation by request id: draining is
// already exact for the sequential requests this harness sends, and could not
// rescue concurrent ones, which would drain each other's records.
type pipelineRecorder struct {
	next slog.Handler

	// Pointers because WithAttrs and WithGroup clone the handler, and every
	// clone has to append to the one buffer Server.do drains.
	mu    *sync.Mutex
	steps *[]string
}

func newPipelineRecorder(next slog.Handler) *pipelineRecorder {
	return &pipelineRecorder{next: next, mu: &sync.Mutex{}, steps: &[]string{}}
}

func (h *pipelineRecorder) Enabled(ctx context.Context, level slog.Level) bool {
	return level == traceLevel || h.next.Enabled(ctx, level)
}

func (h *pipelineRecorder) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == traceEnterMessage {
		h.record(r)
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

// record appends the step named by r.
func (h *pipelineRecorder) record(r slog.Record) {
	var step, name string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case traceStepKey:
			step = a.Value.String()
		case traceNameKey:
			name = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.steps = append(*h.steps, step+"/"+name)
}

func (h *pipelineRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.next = h.next.WithAttrs(attrs)
	return &clone
}

func (h *pipelineRecorder) WithGroup(name string) slog.Handler {
	clone := *h
	clone.next = h.next.WithGroup(name)
	return &clone
}

// drain returns what has been recorded and empties the buffer. Emptying is what
// scopes the report to one request: it runs after every request, so nothing a
// previous one recorded can be reported as this one's.
func (h *pipelineRecorder) drain() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := slices.Clone(*h.steps)
	*h.steps = (*h.steps)[:0]
	return out
}

// PipelineSteps returns the middleware that ran for the most recent request, in
// execution order, each as "Step/middleware" — for example
// "Auth/tenant-guard", or "DB/default" for a step's built-in handler. A
// middleware registered without [maniflex.WithName] is reported as "[unnamed]".
//
// It requires Options.RecordPipeline and reports nothing without it. Requests
// issued concurrently against one Server share the recording, so assert on one
// request at a time or give each its own Server.
func (s *Server) PipelineSteps() []string {
	s.stepsMu.Lock()
	defer s.stepsMu.Unlock()
	return slices.Clone(s.lastSteps)
}

// recordSteps captures what ran for the request that just returned.
func (s *Server) recordSteps() {
	if s.recorder == nil {
		return
	}
	steps := s.recorder.drain()
	s.stepsMu.Lock()
	defer s.stepsMu.Unlock()
	s.lastSteps = steps
}
