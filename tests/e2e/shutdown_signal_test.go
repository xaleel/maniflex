package e2e

// Audit HTTP-6 — gracefulShutdown calls http.Server.Shutdown first, which waits
// for every in-flight request and does not cancel their contexts. A stream is an
// in-flight request that only ends when something tells it to, and the only
// thing that does — Service.Stop, where the documented realtime wiring calls
// Hub.Shutdown — runs after that wait. So one SSE client spent the whole
// ShutdownTimeout before Service.Stop, OnShutdown, the Server.Go drain and the
// in-flight ctx.GoBackground writes got any of it, each then handed an expired
// context.
//
// Server.ShuttingDown is the way out: closed before the wait begins, so a stream
// can return on its own.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// stopWatcher records the context Stop was handed and when it ran.
type stopWatcher struct {
	stopped  chan struct{}
	ctxErr   error
	stopTime time.Time
}

func (s *stopWatcher) Start(context.Context) error { return nil }
func (s *stopWatcher) Stop(ctx context.Context) error {
	s.ctxErr = ctx.Err()
	s.stopTime = time.Now()
	close(s.stopped)
	return nil
}

// streamServer boots a server with one streaming action and one service that
// records what shutdown hands it. watchSignal decides whether that action
// watches ShuttingDown, which is the whole variable under test.
func streamServer(t *testing.T, budget time.Duration, watchSignal bool) (
	base string, svc *stopWatcher, hookErr chan error, streaming chan struct{},
	cancel context.CancelFunc, done chan error,
) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, portStr, _ := net.SplitHostPort(addr)
	port := 0
	fmt.Sscan(portStr, &port)

	svc = &stopWatcher{stopped: make(chan struct{})}
	hookErr = make(chan error, 1)
	streaming = make(chan struct{})

	server := maniflex.New(maniflex.Config{
		Port:            port,
		PathPrefix:      "/api",
		ShutdownTimeout: budget,
		OnShutdown: func(ctx context.Context) error {
			hookErr <- ctx.Err()
			return nil
		},
	})
	server.MustRegister(testutil.DefaultModels()...)
	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)
	server.AddService(svc)

	server.Action(maniflex.ActionConfig{
		Method: http.MethodGet,
		Path:   "/stream",
		Handler: func(ctx *maniflex.ServerContext) error {
			close(streaming)
			if watchSignal {
				select {
				case <-ctx.Ctx.Done():
				case <-ctx.ShuttingDown():
				}
				return nil
			}
			<-ctx.Ctx.Done()
			return nil
		},
	})

	runCtx, cancelFn := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- server.StartWithContext(runCtx) }()
	t.Cleanup(cancelFn)

	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/api/health"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, svc, hookErr, streaming, cancelFn, done
}

func TestShutdownSignal_StreamWatchingItReleasesTheDrain(t *testing.T) {
	t.Parallel()
	const budget = 3 * time.Second

	base, svc, hookErr, streaming, cancel, done := streamServer(t, budget, true)

	go func() {
		if resp, err := http.Get(base + "/api/stream"); err == nil {
			resp.Body.Close()
		}
	}()
	<-streaming

	start := time.Now()
	cancel()

	select {
	case <-svc.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Service.Stop never ran")
	}

	if svc.ctxErr != nil {
		t.Errorf("Service.Stop was handed an expired context (%v); the stream held the "+
			"whole budget and nothing was left for the lifecycle", svc.ctxErr)
	}
	if waited := svc.stopTime.Sub(start); waited > budget/2 {
		t.Errorf("Service.Stop waited %v of the %v budget for one stream that was told to "+
			"wind down", waited, budget)
	}
	select {
	case err := <-hookErr:
		if err != nil {
			t.Errorf("OnShutdown was handed an expired context (%v)", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("OnShutdown never ran")
	}
	<-done
}

// The counterpart, stated so the guarantee is not mistaken for a wider one: a
// handler that ignores the signal still holds the drain, exactly as any slow
// request does. Nothing is cancelled on its behalf.
func TestShutdownSignal_StreamIgnoringItStillHoldsTheDrain(t *testing.T) {
	t.Parallel()
	const budget = 1500 * time.Millisecond

	base, svc, _, streaming, cancel, done := streamServer(t, budget, false)

	go func() {
		if resp, err := http.Get(base + "/api/stream"); err == nil {
			resp.Body.Close()
		}
	}()
	<-streaming

	start := time.Now()
	cancel()

	select {
	case <-svc.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Service.Stop never ran")
	}
	if waited := svc.stopTime.Sub(start); waited < budget/2 {
		t.Errorf("Service.Stop ran after only %v; a handler that ignores ShuttingDown is "+
			"supposed to keep the drain it always had", waited)
	}
	<-done
}

// The signal is closed before the drain begins, not after it, which is the
// property the whole fix rests on.
func TestShutdownSignal_ClosesBeforeTheDrain(t *testing.T) {
	t.Parallel()

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", ShutdownTimeout: time.Second})
	server.MustRegister(testutil.DefaultModels()...)

	select {
	case <-server.ShuttingDown():
		t.Fatal("ShuttingDown is closed on a server that has not been asked to stop")
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-server.ShuttingDown():
	default:
		t.Fatal("ShuttingDown is still open after Shutdown returned")
	}
}
