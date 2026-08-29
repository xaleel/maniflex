package maniflextest_test

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/maniflextest"
)

func passthrough(ctx *maniflex.ServerContext, next func() error) error { return next() }

// stepIndex reports where name appears in steps, or -1.
func stepIndex(steps []string, name string) int {
	return slices.Index(steps, name)
}

func TestPipelineSteps_ReportsMiddlewareInExecutionOrder(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models:         []any{Widget{}},
		RecordPipeline: true,
		Setup: func(app *maniflex.Server) {
			// Registered service-first to prove the report follows execution
			// order (Auth → … → Service), not registration order.
			app.Pipeline.Service.Register(passthrough, maniflex.WithName("business-rules"))
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)

	steps := server.PipelineSteps()
	auth := stepIndex(steps, "Auth/tenant-guard")
	service := stepIndex(steps, "Service/business-rules")
	if auth < 0 || service < 0 {
		t.Fatalf("PipelineSteps() = %v, want it to name both middlewares", steps)
	}
	if auth > service {
		t.Errorf("Auth middleware reported after Service middleware: %v", steps)
	}
}

func TestPipelineSteps_RecordsEachMiddlewareOnce(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models:         []any{Widget{}},
		RecordPipeline: true,
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)

	// The framework logs a middleware twice per request, entering and leaving
	// it. The report is of what ran, in the order it ran, so each middleware
	// belongs in it once.
	seen := map[string]int{}
	for _, step := range server.PipelineSteps() {
		seen[step]++
	}
	if n := seen["Auth/tenant-guard"]; n != 1 {
		t.Errorf("Auth/tenant-guard appears %d times, want 1: %v", n, server.PipelineSteps())
	}
}

func TestPipelineSteps_IsEmptyWhenRecordingIsNotRequested(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models: []any{Widget{}},
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)

	if steps := server.PipelineSteps(); len(steps) != 0 {
		t.Errorf("PipelineSteps() = %v, want empty without Options.RecordPipeline", steps)
	}
}

func TestPipelineSteps_ReportsTheMostRecentRequestOnly(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models:         []any{Widget{}},
		RecordPipeline: true,
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)
	first := len(server.PipelineSteps())
	server.GET("/widgets").AssertStatus(http.StatusOK)
	second := len(server.PipelineSteps())

	if first == 0 {
		t.Fatalf("first request recorded nothing")
	}
	if second != first {
		t.Errorf("second request reported %d steps, want %d — the report accumulated across requests", second, first)
	}
}

func TestPipelineSteps_DoNotLeakIntoARequestThatRanNoPipeline(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models:         []any{Widget{}},
		RecordPipeline: true,
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)
	if len(server.PipelineSteps()) == 0 {
		t.Fatalf("the model request recorded nothing")
	}

	// An unrouted path answers 404 without entering the pipeline and without a
	// request id, so there is no id to scope the report by. A buffer left
	// standing from the previous request is what would be reported instead.
	server.DoRoot(http.MethodGet, "/no-such-route", nil).AssertStatus(http.StatusNotFound)

	if steps := server.PipelineSteps(); len(steps) != 0 {
		t.Errorf("PipelineSteps() = %v after a request that ran no middleware, want empty", steps)
	}
}

// recordingHandler is a consumer-supplied slog handler.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}

func TestPipelineSteps_KeepsTheConfiguredLogger(t *testing.T) {
	own := &recordingHandler{}
	server := maniflextest.New(t, maniflextest.Options{
		Config:         maniflex.Config{Logger: slog.New(own)},
		Models:         []any{Widget{}},
		RecordPipeline: true,
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(passthrough, maniflex.WithName("tenant-guard"))
		},
	})

	server.GET("/widgets").AssertStatus(http.StatusOK)

	if len(server.PipelineSteps()) == 0 {
		t.Errorf("PipelineSteps() is empty; recording did not survive a configured Logger")
	}
	if !own.saw("middleware enter") {
		t.Errorf("the configured Logger stopped receiving records; recording replaced it instead of teeing")
	}
}
