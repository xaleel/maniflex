package e2e

// Shutdown runs on one budget. The HTTP drain, Service.Stop, the OnShutdown hook
// and the goroutine drain all share Config.ShutdownTimeout (or the deadline given
// to Server.Shutdown) — they used to mint a fresh full-length window each, so the
// phases were additive and a shutdown could take ~3× the timeout the docs promise,
// long enough for an orchestrator to SIGKILL mid-stop (BUG-11).

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// blockingStopService never finishes Stop on its own — it waits for the ctx it is
// given, so whatever budget it receives is the budget it burns.
type blockingStopService struct {
	stopDeadlineIn atomic.Int64 // ns left on the ctx when Stop was entered
	sawDeadline    atomic.Bool
}

func (s *blockingStopService) Start(context.Context) error { return nil }

func (s *blockingStopService) Stop(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		s.sawDeadline.Store(true)
		s.stopDeadlineIn.Store(int64(time.Until(dl)))
	}
	<-ctx.Done()
	return nil
}

// An explicit Shutdown(ctx) deadline must bound every phase, not just the HTTP
// drain. The lifecycle phase used to ignore it and mint its own ShutdownTimeout.
func TestShutdown_ExplicitDeadlineBoundsServiceStop(t *testing.T) {
	svc := &blockingStopService{}

	lsrv := newLifecycleServer(t, func(s *maniflex.Server) {
		s.AddService(svc)
	}, maniflex.Config{
		ShutdownTimeout: 3 * time.Second, // generous — the caller's deadline must win
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := lsrv.server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Errorf("Shutdown took %v with a 200ms deadline — the service stop ran on its own budget", elapsed)
	}
	if !svc.sawDeadline.Load() {
		t.Fatal("Service.Stop received a ctx with no deadline")
	}
	if got := time.Duration(svc.stopDeadlineIn.Load()); got > time.Second {
		t.Errorf("Service.Stop got %v of budget, want ~200ms — it was handed a fresh ShutdownTimeout", got)
	}
}

// The phases are not additive: a Service.Stop that burns the whole window leaves
// nothing for the OnShutdown hook, which therefore sees an already-expired ctx
// rather than a fresh full-length one.
func TestShutdown_PhasesShareOneBudget(t *testing.T) {
	svc := &blockingStopService{}
	var hookRan, hookCtxExpired atomic.Bool

	lsrv := newLifecycleServer(t, func(s *maniflex.Server) {
		s.AddService(svc)
	}, maniflex.Config{
		ShutdownTimeout: time.Second,
		OnShutdown: func(ctx context.Context) error {
			hookRan.Store(true)
			hookCtxExpired.Store(ctx.Err() != nil)
			return nil
		},
	})

	start := time.Now()
	lsrv.cancel() // signal path: gracefulShutdown with a single ShutdownTimeout window
	select {
	case <-lsrv.done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
	elapsed := time.Since(start)

	if !hookRan.Load() {
		t.Fatal("OnShutdown never ran")
	}
	// Stop blocked until the shared budget ran out, so the hook inherits an
	// exhausted ctx. A fresh one here is the bug: the budgets were additive.
	if !hookCtxExpired.Load() {
		t.Error("OnShutdown got a fresh budget after Service.Stop consumed the window")
	}
	if elapsed > 1700*time.Millisecond {
		t.Errorf("shutdown took %v with a 1s timeout — phases are still additive", elapsed)
	}
}

// A connection that is accepted and then says nothing must not spend the
// shutdown budget. net/http will not count one as idle until it has sat there
// for over five seconds (Go issue 22682), so with the production
// ReadHeaderTimeout of 10s a single half-open connection makes
// http.Server.Shutdown wait out any budget below 5s and report a failed drain —
// for a drain with nothing in it. That is what broke the race lane in CI on
// TestEmitDrain_ShutdownWaitsForInFlightPublish, whose budget is exactly 5s:
// not a race, a coin landing badly on a slower runner.
//
// newLifecycleServer therefore gives these servers a short ReadHeaderTimeout, so
// net/http hangs up on such a connection long before any budget here elapses.
// This pins that, because the symptom is a timeout in an unrelated test and the
// cause is invisible from it.
func TestShutdown_HalfOpenConnectionDoesNotSpendTheBudget(t *testing.T) {
	lsrv := newLifecycleServer(t, nil, maniflex.Config{ShutdownTimeout: 30 * time.Second})

	// Accepted by the server, not one byte written: StateNew, for as long as the
	// server tolerates it.
	conn, err := net.Dial("tcp", strings.TrimPrefix(lsrv.url, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond) // let the server accept it

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := lsrv.server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v — a connection that never sent a request header "+
			"consumed the whole budget; every lifecycle test with a budget under "+
			"5s fails this way, and the failure names the drain rather than the "+
			"connection", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("shutdown took %v for a server with no in-flight work: the HTTP "+
			"drain is still waiting on the half-open connection", elapsed)
	}
}
