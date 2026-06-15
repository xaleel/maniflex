package maniflex

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

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
		// with no shutdown coupling.
		go fn(context.Background())
		return
	}
	b.wg.Add(1)
	b.inFlight.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.inFlight.Add(-1)
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
