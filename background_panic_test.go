package maniflex

// The request goroutine is carefully recovered by PanicRecoverer, but the
// goroutines the framework spawns on an application's behalf were not: a panic
// in a ctx.GoBackground task (an audit write, a cache invalidation, a file
// cleanup) or in a Server.Go loop unwound out of an unrecovered goroutine, and
// the Go runtime then killed the whole binary — a worse outcome than the request
// panic the framework goes out of its way to contain (audit H1).
//
// These tests fail by *crashing the test binary* before the fix, which is
// exactly the failure mode being fixed.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer is a concurrency-safe sink for a slog handler. The panic is
// recovered and logged on the background goroutine; the assertions read it from
// the test goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func captureLogger() (*slog.Logger, *safeBuffer) {
	sink := &safeBuffer{}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelError})), sink
}

// A panicking ctx.GoBackground task must not take the process with it. The
// runner still has to account for it — Wait reports a clean drain — and the
// panic has to be reported, because a contained panic nobody hears about is a
// silent loss of the audit write that panicked.
func TestGoBackground_PanicIsContainedAndLogged(t *testing.T) {
	logger, sink := captureLogger()
	srv := New(Config{PanicLogger: logger})

	srv.steps.bg.Go(func(context.Context) {
		panic("boom from a background task")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := srv.steps.bg.Wait(ctx); n != 0 {
		t.Fatalf("Wait reported %d task(s) still in flight, want 0 — a panicking task must "+
			"still be accounted for by the drain", n)
	}

	got := sink.String()
	if !strings.Contains(got, "boom from a background task") {
		t.Fatalf("panic value missing from the log; PanicLogger got:\n%s", got)
	}
	if !strings.Contains(got, "stack") {
		t.Fatalf("recovered background panic logged without a stack trace, which is the only "+
			"way to find where it came from; PanicLogger got:\n%s", got)
	}
}

// The same guarantee for Server.Go, whose goroutines are application-owned
// long-running loops rather than one-shot tasks.
func TestServerGo_PanicIsContainedAndLogged(t *testing.T) {
	logger, sink := captureLogger()
	srv := New(Config{PanicLogger: logger})

	srv.Go(func(context.Context) {
		panic("boom from a Server.Go loop")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !srv.lifecycle.drain(ctx) {
		t.Fatal("drain timed out waiting for a panicking Server.Go goroutine")
	}

	got := sink.String()
	if !strings.Contains(got, "boom from a Server.Go loop") {
		t.Fatalf("panic value missing from the log; PanicLogger got:\n%s", got)
	}
}

// Containment keeps the process alive, which is the point — but it also turns a
// loud crash into silent degradation, so an application needs a way to notice.
// Config.OnBackgroundPanic is that lever: it gets the recovered value and the
// stack, so an app can alert, count it, or exit deliberately.
func TestOnBackgroundPanic_HookReceivesPanicAndStack(t *testing.T) {
	var (
		mu     sync.Mutex
		calls  int
		gotRec any
		gotStk []byte
	)
	logger, _ := captureLogger()
	srv := New(Config{
		PanicLogger: logger,
		OnBackgroundPanic: func(rec any, stack []byte) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			gotRec, gotStk = rec, stack
		},
	})

	srv.steps.bg.Go(func(context.Context) {
		panic("boom the hook should see")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := srv.steps.bg.Wait(ctx); n != 0 {
		t.Fatalf("Wait reported %d task(s) in flight, want 0", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("OnBackgroundPanic called %d times, want exactly 1", calls)
	}
	if gotRec != "boom the hook should see" {
		t.Fatalf("hook got recovered value %#v, want the panic value", gotRec)
	}
	if !strings.Contains(string(gotStk), "TestOnBackgroundPanic_HookReceivesPanicAndStack") {
		t.Fatalf("hook got a stack that does not reach the panicking call site:\n%s", gotStk)
	}
}

// The hook covers Server.Go as well — that is the path where containment is
// most costly, because a dead supervised loop leaves the process serving while
// the work it was doing has stopped.
func TestOnBackgroundPanic_HookCoversServerGo(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	logger, _ := captureLogger()
	srv := New(Config{
		PanicLogger: logger,
		OnBackgroundPanic: func(any, []byte) {
			mu.Lock()
			defer mu.Unlock()
			calls++
		},
	})

	srv.Go(func(context.Context) {
		panic("boom from a supervised loop")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !srv.lifecycle.drain(ctx) {
		t.Fatal("drain timed out waiting for a panicking Server.Go goroutine")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("OnBackgroundPanic called %d times for a Server.Go panic, want exactly 1", calls)
	}
}

// The nil-runner path — a ServerContext synthesised without server wiring
// (NewBackground, an older test, a custom action wrapper) — runs fn on a bare
// goroutine. It has no Config to consult, so it reports through slog.Default(),
// but it must not be the one path that still kills the process.
func TestGoBackground_NilRunnerPanicIsContained(t *testing.T) {
	logger, sink := captureLogger()
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	var b *backgroundRunner // exactly what ctx.bg is on a NewBackground context
	b.Go(func(context.Context) {
		panic("boom from an untracked task")
	})

	// The log line is written by the recover, so seeing it *is* the proof that
	// recovery ran — no sleep-and-hope.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sink.String(), "boom from an untracked task") {
		if time.Now().After(deadline) {
			t.Fatalf("untracked background panic was never recovered or reported; log:\n%s",
				sink.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
