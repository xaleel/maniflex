package maniflex

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// recoverBackgroundPanic contains a panic raised by application code running on
// a framework-owned goroutine, and reports it through logger.
//
// The request goroutine has PanicRecoverer; the goroutines the framework spawns
// on an application's behalf had nothing, so a panic in a ctx.GoBackground task
// or a Server.Go loop unwound into the runtime and killed the process — a worse
// outcome than the request panic the framework goes out of its way to contain
// (audit H1).
//
// Containment alone would be a silent swallow, so the panic is always reported:
// the stack is captured here, at the top of the unwind, because it shrinks as
// the goroutine unwinds any further. task names the goroutine's origin so the
// callers stay distinguishable in the log.
//
// logger may be nil, in which case slog.Default() is used — the same fallback
// PanicRecoverer makes. onPanic is Config.OnBackgroundPanic and may be nil; it
// runs after the log so a hook that exits the process still leaves the record
// behind that says why.
func recoverBackgroundPanic(logger *slog.Logger, onPanic func(any, []byte), task string) {
	rec := recover()
	if rec == nil {
		return
	}
	stack := debug.Stack()
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(context.Background(), slog.LevelError,
		"maniflex: recovered from panic in background goroutine",
		slog.String("task", task),
		slog.String("panic", panicString(rec)),
		slog.String("stack", string(stack)),
	)
	if onPanic != nil {
		onPanic(rec, stack)
	}
}

// panicString renders a recovered panic value for a log attribute.
func panicString(rec any) string {
	switch v := rec.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return fmt.Sprintf("%+v", v)
	}
}

// backgroundRunner tracks goroutines spawned by middleware (audit-log
// writes, cache invalidations, async file deletes) so Server.Shutdown can
// wait for them to complete rather than letting the process exit mid-write.
//
// Roadmap §11B.6: previously these helpers used `go func() { sink.Write(
// context.Background(), ...) }()`, which meant audit records could be
// truncated or lost when the binary exited just after returning the HTTP
// response.
type backgroundRunner struct {
	wg       sync.WaitGroup
	inFlight atomic.Int64

	rootCtx context.Context
	cancel  context.CancelFunc

	// panicLogger reports a panic that escaped a background task. Set from
	// Config.PanicLogger in New; nil falls back to slog.Default(). onPanic is
	// Config.OnBackgroundPanic, the application's chance to act on one.
	panicLogger *slog.Logger
	onPanic     func(recovered any, stack []byte)
}

func newBackgroundRunner() *backgroundRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundRunner{rootCtx: ctx, cancel: cancel}
}

// Go schedules fn on a fresh goroutine and tracks it for Shutdown. The ctx
// passed to fn is derived from the runner's root context so request-scoped
// cancellation (the HTTP request has already returned) doesn't kill the
// background write. The ctx IS cancelled by Wait when its deadline hits, so
// well-behaved writers honour the cancellation and exit promptly.
func (b *backgroundRunner) Go(fn func(context.Context)) {
	if b == nil {
		// Safety net: callers that synthesise a ServerContext without a
		// runner (older tests, custom action wrappers) get a plain goroutine
		// with no shutdown coupling. There is no Config to consult here, so the
		// panic is reported through slog.Default().
		go func() {
			defer recoverBackgroundPanic(nil, nil, "ctx.GoBackground (untracked)")
			fn(context.Background())
		}()
		return
	}
	b.wg.Add(1)
	b.inFlight.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.inFlight.Add(-1)
		// Registered last so it unwinds first: the panic is recovered before
		// the accounting defers run, so a panicking task still drains cleanly.
		defer recoverBackgroundPanic(b.panicLogger, b.onPanic, "ctx.GoBackground")
		fn(b.rootCtx)
	}()
}

// Wait blocks until all tracked goroutines have returned, ctx is cancelled,
// or both. On ctx-cancel it cancels the runner's root context so in-flight
// writers see the signal and exit, then waits an additional 50ms grace for
// them to drain. Returns the number of goroutines that were still in flight
// when Wait gave up (0 = clean drain).
func (b *backgroundRunner) Wait(ctx context.Context) int64 {
	if b == nil {
		return 0
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return 0
	case <-ctx.Done():
	}

	// Deadline hit: signal in-flight tasks and give them a brief grace.
	b.cancel()
	select {
	case <-done:
		return 0
	case <-time.After(50 * time.Millisecond):
	}
	return b.inFlight.Load()
}
