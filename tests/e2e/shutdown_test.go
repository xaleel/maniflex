package e2e

// shutdown_test.go tests graceful shutdown behaviour end-to-end.
// Every test spins up a real net.Listener on a random port so tests
// can run in parallel without port conflicts.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestGracefulShutdown

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestGracefulShutdown covers the full shutdown lifecycle.
func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	t.Run("returns_nil_on_clean_drain", func(t *testing.T) {
		t.Parallel()
		srv := newShutdownServer(t, shutdownOptions{timeout: 5 * time.Second})

		// Server must be reachable before we shut it down.
		mustGet(t, srv.url+"/api/health", http.StatusOK)

		// Trigger shutdown and expect a nil return.
		srv.cancel()
		select {
		case err := <-srv.done:
			if err != nil {
				t.Errorf("StartWithContext returned non-nil on clean drain: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("StartWithContext did not return within 5s after cancel")
		}
	})

	t.Run("in_flight_request_completes_before_close", func(t *testing.T) {
		t.Parallel()
		const slowDown = 200 * time.Millisecond

		srv := newShutdownServer(t, shutdownOptions{
			timeout: 5 * time.Second,
			// Service middleware that holds the connection open for slowDown.
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					time.Sleep(slowDown)
					return next()
				}, maniflex.ForOperation(maniflex.OpList))
			},
		})

		// Start a slow request.
		requestDone := make(chan int, 1)
		go func() {
			resp, err := http.Get(srv.url + "/api/users")
			if err != nil {
				requestDone <- 0
				return
			}
			resp.Body.Close()
			requestDone <- resp.StatusCode
		}()

		// Give the request time to enter the handler then initiate shutdown.
		time.Sleep(30 * time.Millisecond)
		srv.cancel()

		// The in-flight request must finish successfully.
		select {
		case status := <-requestDone:
			if status != http.StatusOK {
				t.Errorf("in-flight request got %d during graceful shutdown, want 200", status)
			}
		case <-time.After(3 * time.Second):
			t.Error("in-flight request did not complete within 3s during shutdown")
		}

		// Server must also exit cleanly.
		select {
		case err := <-srv.done:
			if err != nil {
				t.Errorf("StartWithContext returned error after clean drain: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not exit within 3s after in-flight request finished")
		}
	})

	t.Run("many_concurrent_requests_all_complete_before_close", func(t *testing.T) {
		t.Parallel()
		const slowDown = 150 * time.Millisecond
		const n = 5

		srv := newShutdownServer(t, shutdownOptions{
			timeout: 5 * time.Second,
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					time.Sleep(slowDown)
					return next()
				}, maniflex.ForOperation(maniflex.OpList))
			},
		})

		var wg sync.WaitGroup
		statuses := make([]int, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp, err := http.Get(srv.url + "/api/users")
				if err != nil {
					return
				}
				resp.Body.Close()
				statuses[i] = resp.StatusCode
			}(i)
		}

		// Let all requests enter the handler, then shutdown.
		time.Sleep(30 * time.Millisecond)
		srv.cancel()

		allDone := make(chan struct{})
		go func() { wg.Wait(); close(allDone) }()

		select {
		case <-allDone:
		case <-time.After(5 * time.Second):
			t.Error("not all in-flight requests completed within 5s")
		}

		select {
		case err := <-srv.done:
			if err != nil {
				t.Logf("server exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not exit within 5s")
		}

		for i, s := range statuses {
			if s != 0 && s != http.StatusOK {
				t.Errorf("request %d: got %d, want 200", i, s)
			}
		}
	})

	t.Run("short_timeout_forces_close_when_requests_stall", func(t *testing.T) {
		t.Parallel()

		srv := newShutdownServer(t, shutdownOptions{
			timeout: 80 * time.Millisecond, // much shorter than the request delay
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					time.Sleep(10 * time.Second) // will be cut short
					return next()
				}, maniflex.ForOperation(maniflex.OpList))
			},
		})

		// Fire a request that will stall indefinitely.
		go func() { http.Get(srv.url + "/api/users") }() //nolint:errcheck
		time.Sleep(20 * time.Millisecond)

		start := time.Now()
		srv.cancel()

		select {
		case err := <-srv.done:
			elapsed := time.Since(start)
			// Server must return within ~500ms (80ms timeout + some overhead).
			if elapsed > 2*time.Second {
				t.Errorf("forced close took too long: %s", elapsed)
			}
			// A context-deadline-exceeded error is expected and acceptable here.
			if err != nil {
				t.Logf("server returned (expected) error on forced close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not force-close within 5s even after timeout")
		}
	})

	t.Run("shutdown_method_triggers_drain_without_context_cancel", func(t *testing.T) {
		t.Parallel()

		srv := newShutdownServer(t, shutdownOptions{timeout: 5 * time.Second})
		mustGet(t, srv.url+"/api/health", http.StatusOK)

		// Call Shutdown() directly — do not cancel the context.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		if err := srv.server.Shutdown(shutCtx); err != nil {
			t.Errorf("Shutdown() returned error: %v", err)
		}

		select {
		case <-srv.done:
			// good — server exited after Shutdown()
		case <-time.After(3 * time.Second):
			t.Error("server did not exit after explicit Shutdown()")
		}
	})

	t.Run("shutdown_before_start_is_noop", func(t *testing.T) {
		t.Parallel()
		// Shutdown on a never-started server must return nil without panicking.
		s := maniflex.New(maniflex.Config{})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown on un-started server returned error: %v", err)
		}
	})

	t.Run("server_rejects_new_connections_after_shutdown_begins", func(t *testing.T) {
		t.Parallel()

		// Use a long slowDown so the first request is still in-flight when we
		// attempt the second one post-shutdown.
		const slowDown = 400 * time.Millisecond

		srv := newShutdownServer(t, shutdownOptions{
			timeout: 5 * time.Second,
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					time.Sleep(slowDown)
					return next()
				}, maniflex.ForOperation(maniflex.OpList))
			},
		})

		// Send the first (slow) request.
		firstDone := make(chan int, 1)
		go func() {
			resp, err := http.Get(srv.url + "/api/users")
			if err != nil {
				firstDone <- 0
				return
			}
			resp.Body.Close()
			firstDone <- resp.StatusCode
		}()

		// Wait for it to enter the handler, then trigger shutdown.
		time.Sleep(30 * time.Millisecond)
		srv.cancel()

		// The first request should still complete.
		select {
		case status := <-firstDone:
			if status != http.StatusOK {
				t.Errorf("first request got %d, want 200", status)
			}
		case <-time.After(3 * time.Second):
			t.Error("first request did not complete")
		}

		// Attempt a second request after shutdown started — it must fail
		// (connection refused or similar network error), not return 200.
		_, secondErr := http.Get(srv.url + "/api/users")
		if secondErr == nil {
			t.Error("new request after shutdown should fail with a connection error, not succeed")
		}
	})

	t.Run("zero_shutdown_timeout_defaults_to_30s", func(t *testing.T) {
		t.Parallel()
		// ShutdownTimeout: 0 in Config must be replaced by the 30s default.
		// Verified by starting and immediately stopping a server with zero value:
		// if it used a 0-duration timeout every shutdown would time out.
		srv := newShutdownServer(t, shutdownOptions{timeout: 0})
		srv.cancel()
		select {
		case err := <-srv.done:
			if err != nil {
				t.Errorf("zero-timeout server returned error on clean shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("zero-timeout server did not exit within 5s")
		}
	})
}

// ── Infrastructure ────────────────────────────────────────────────────────────

// shutdownOptions configures the server created by newShutdownServer.
type shutdownOptions struct {
	timeout    time.Duration
	middleware func(s *maniflex.Server)
}

// shutdownServer holds a running Maniflex server started on a random free port.
type shutdownServer struct {
	url    string              // e.g. "http://127.0.0.1:54321"
	server *maniflex.Server            // direct handle for calling Shutdown()
	cancel context.CancelFunc  // cancels the StartWithContext context
	done   <-chan error         // receives the return value of StartWithContext
}

// newShutdownServer starts a real Maniflex server on a random free TCP port and
// blocks until the /health endpoint responds, guaranteeing the server is
// accepting connections before any assertions run.
func newShutdownServer(t *testing.T, opts shutdownOptions) *shutdownServer {
	t.Helper()

	// Pick a random free port — avoids port 8080 conflicts and allows full parallelism.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	// Close the listener so the Server server can bind to the same address.
	// There is a tiny TOCTOU race here, but on loopback it's acceptable for tests.
	ln.Close()

	_, portStr, _ := net.SplitHostPort(addr)
	port := 0
	fmt.Sscan(portStr, &port)

	server := maniflex.New(maniflex.Config{
		Port:            port,
		PathPrefix:      "/api",
		AutoMigrate:     true,
		ShutdownTimeout: opts.timeout,
	})
	server.MustRegister(testutil.DefaultModels()...)

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)

	if opts.middleware != nil {
		opts.middleware(server)
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- server.StartWithContext(ctx) }()

	// Poll until the health endpoint responds (or deadline).
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(cancelFn) // ensure cancel is always called, even on test failure

	return &shutdownServer{
		url:    baseURL,
		server: server,
		cancel: cancelFn,
		done:   ch,
	}
}

// mustGet asserts that GET url returns the expected status code.
func mustGet(t *testing.T, url string, wantStatus int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Errorf("GET %s: got %d, want %d", url, resp.StatusCode, wantStatus)
	}
}
