package e2e

// Audit EV-6 (High): nothing drained in-flight event deliveries at shutdown.
// events.Emit published from an untracked goroutine on context.Background(), so
// a publish still in flight when the process exited was simply lost — and the
// ctx-cancellation path the goroutine appeared to have could never fire, because
// Background is never cancelled.
//
// At-least-once therefore degraded to at-most-once at exactly the moment a
// deploy or a scale-down makes deliveries most likely to be interrupted.
//
// The Server already tracks fire-and-forget work for audit writes and cache
// invalidations (backgroundRunner, reached through ctx.GoBackground) and waits
// on it during Shutdown. Emit was simply not using it.
//
// This test starts a *real* server. A Handler()-only server never marks itself
// started, and Shutdown documents an early return for one that was never started
// — so a testutil server would drain nothing and the test would fail whatever
// the code did.
//
//	go test ./tests/e2e/... -run TestEmitDrain

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
)

// slowBus takes a measurable moment to publish, standing in for a broker round
// trip. It records completions, so a publish abandoned mid-flight shows up as a
// missing count rather than as a flake.
type slowBus struct {
	delay time.Duration

	mu        sync.Mutex
	started   int
	finished  int
	sawCancel bool
}

func (b *slowBus) Publish(ctx context.Context, _ events.Event) error {
	b.mu.Lock()
	b.started++
	b.mu.Unlock()

	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		// Reaching this at all means the publish saw a cancellation signal —
		// impossible on context.Background(), which is half the finding.
		b.mu.Lock()
		b.sawCancel = true
		b.mu.Unlock()
		return ctx.Err()
	}

	b.mu.Lock()
	b.finished++
	b.mu.Unlock()
	return nil
}

func (b *slowBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (b *slowBus) Close() error { return nil }

func (b *slowBus) counts() (started, finished int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started, b.finished
}

func (b *slowBus) cancelled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sawCancel
}

// waitStarted blocks until a publish is actually in flight, so the shutdown
// under test is not racing the delivery goroutine's start.
func (b *slowBus) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := b.counts(); s > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("precondition: no publish ever started; the fixture proves nothing")
}

// emitDrainServer boots a real server with Emit wired to bus, and creates one
// record so a publish is in flight when the caller shuts down.
func emitDrainServer(t *testing.T, bus events.Publisher, shutdownTimeout time.Duration) *lifecycleServer {
	t.Helper()
	lsrv := newLifecycleServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(
			events.Emit(bus),
			maniflex.ForOperation(maniflex.OpCreate),
			maniflex.AtPosition(maniflex.After))
	}, maniflex.Config{ShutdownTimeout: shutdownTimeout})

	body := bytes.NewBufferString(`{"name":"drain","email":"drain@x.com","password":"pw","role":"viewer"}`)
	resp, err := http.Post(lsrv.url+"/api/users", "application/json", body)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, want 201", resp.StatusCode)
	}
	return lsrv
}

// TestEmitDrain_ShutdownWaitsForInFlightPublish is the EV-6 regression for the
// Emit half: a publish in flight when Shutdown is called must complete.
func TestEmitDrain_ShutdownWaitsForInFlightPublish(t *testing.T) {
	bus := &slowBus{delay: 200 * time.Millisecond}
	lsrv := emitDrainServer(t, bus, 5*time.Second)
	bus.waitStarted(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lsrv.server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	started, finished := bus.counts()
	if finished != started {
		t.Errorf("%d publish(es) started but only %d finished: shutdown abandoned in-flight "+
			"deliveries, so at-least-once degrades to at-most-once exactly at deploy time",
			started, finished)
	}
	if bus.cancelled() {
		t.Error("the publish was cancelled despite finishing well inside the shutdown budget")
	}
}

// The other half: the goroutine ran on context.Background(), so the cancellation
// it appeared to honour could never arrive. A publish outliving the shutdown
// budget must actually be signalled, not merely outlived.
func TestEmitDrain_PublishSeesShutdownCancellation(t *testing.T) {
	bus := &slowBus{delay: 30 * time.Second} // far beyond the budget below
	lsrv := emitDrainServer(t, bus, 300*time.Millisecond)
	bus.waitStarted(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = lsrv.server.Shutdown(ctx)

	if !bus.cancelled() {
		t.Error("the publish never observed a cancellation signal: it is running on a context " +
			"nothing can cancel, so shutdown can only outlive it, never ask it to stop")
	}
}
