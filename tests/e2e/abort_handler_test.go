package e2e

// http.ErrAbortHandler is not a failure: it is how a handler says "I am
// abandoning this response on purpose", and net/http's contract is to close the
// connection without logging. PanicRecoverer recovered it like any other panic —
// logging a phantom panic at ERROR and appending a 500 PANIC JSON body to a
// response that was already half-written and half-sent (BUG-19).
//
// httputil.ReverseProxy panics with it when an upstream dies mid-stream, which is
// exactly the shape reproduced here: bytes on the wire, then the abort.

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestAbortHandler_NotRecovered(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}

	srv := testutil.NewServer(t, testutil.Options{
		PanicLogger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})),
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/proxy",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Writer.Header().Set("Content-Type", "text/plain")
					ctx.Writer.WriteHeader(http.StatusOK)
					_, _ = ctx.Writer.Write([]byte("partial payload"))
					if f, ok := ctx.Writer.(http.Flusher); ok {
						f.Flush() // the client already has these bytes
					}
					panic(http.ErrAbortHandler) // upstream died mid-stream
				},
			})
		},
	})

	resp, err := srv.Client().Get(srv.URL("/api/proxy"))
	if err != nil {
		t.Fatalf("request: %v", err) // the headers did go out
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)

	// The connection must be dropped: an aborted response is truncated, so
	// reading it out fails rather than completing cleanly.
	if readErr == nil {
		t.Errorf("body read completed cleanly (%q) — the aborted response was served as if whole", body)
	}
	// And whatever did arrive must not have a JSON error envelope stapled onto it.
	if strings.Contains(string(body), "PANIC") {
		t.Errorf("a 500 PANIC envelope was appended to a half-written response: %q", body)
	}
	if got := logs.String(); got != "" {
		t.Errorf("a deliberate abort was logged as a panic:\n%s", got)
	}
}

// The guard on the other side: a real panic is still recovered, logged, and
// answered with 500 PANIC.
func TestAbortHandler_RealPanicStillRecovered(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}

	srv := testutil.NewServer(t, testutil.Options{
		PanicLogger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})),
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				panic("genuinely broken")
			})
		},
	})

	resp := srv.GET("/users")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "PANIC" {
		t.Errorf("error code: got %q, want PANIC", code)
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("a real panic must still be logged; logs:\n%s", logs.String())
	}
}
