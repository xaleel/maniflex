package e2e

// service_test.go covers background-service supervision and lifecycle hooks
// (roadmap §2.9): AddService start/stop ordering, Server.Go draining, and the
// Config.OnStart / OnShutdown hooks.
//
//	go test ./tests/e2e/... -run TestServiceSupervision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maniflex"
	"maniflex/db/sqlite"
	"maniflex/tests/e2e/testutil"
)

// recordingService records its Start/Stop into a shared, ordered log.
type recordingService struct {
	name     string
	log      *eventLog
	startErr error
}

func (s *recordingService) Start(ctx context.Context) error {
	s.log.add("start:" + s.name)
	return s.startErr
}

func (s *recordingService) Stop(ctx context.Context) error {
	s.log.add("stop:" + s.name)
	return nil
}

// eventLog is a goroutine-safe ordered string log.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

func TestServiceSupervision(t *testing.T) {
	t.Parallel()

	t.Run("services_start_in_order_and_stop_in_reverse", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		srv := newLifecycleServer(t, func(s *maniflex.Server) {
			s.AddService(&recordingService{name: "a", log: log})
			s.AddService(&recordingService{name: "b", log: log})
			s.AddService(&recordingService{name: "c", log: log})
		}, maniflex.Config{})

		mustGet(t, srv.url+"/api/health", http.StatusOK)

		// All three must have started, in registration order, before serving.
		if got, want := srv.log(log, "start:a", "start:b", "start:c"), true; got != want {
			t.Fatalf("start order wrong: %v", log.snapshot())
		}

		srv.cancel()
		<-srv.done

		want := []string{"start:a", "start:b", "start:c", "stop:c", "stop:b", "stop:a"}
		if got := log.snapshot(); !equalStrings(got, want) {
			t.Errorf("lifecycle order:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("start_error_aborts_boot_and_rolls_back", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		srv := maniflex.New(testServerConfig(t))
		srv.MustRegister(testutil.DefaultModels()...)
		db, err := sqlite.Open(":memory:", srv.Registry())
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		srv.SetDB(db)

		boom := errors.New("boom")
		srv.AddService(&recordingService{name: "a", log: log})
		srv.AddService(&recordingService{name: "b", log: log, startErr: boom})
		srv.AddService(&recordingService{name: "c", log: log}) // never reached

		err = srv.StartWithContext(context.Background())
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("StartWithContext: got %v, want wrapping %v", err, boom)
		}

		// a started, b failed; a must be rolled back, c never started.
		want := []string{"start:a", "start:b", "stop:a"}
		if got := log.snapshot(); !equalStrings(got, want) {
			t.Errorf("rollback order:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("server_go_ctx_cancelled_at_shutdown_and_drained", func(t *testing.T) {
		t.Parallel()
		var (
			started  = make(chan struct{})
			finished atomic.Bool
		)
		srv := newLifecycleServer(t, func(s *maniflex.Server) {
			s.Go(func(ctx context.Context) {
				close(started)
				<-ctx.Done() // must observe cancellation at shutdown
				time.Sleep(20 * time.Millisecond)
				finished.Store(true)
			})
		}, maniflex.Config{})

		<-started
		mustGet(t, srv.url+"/api/health", http.StatusOK)

		srv.cancel()
		select {
		case <-srv.done:
		case <-time.After(5 * time.Second):
			t.Fatal("server did not exit")
		}
		if !finished.Load() {
			t.Error("Server.Go goroutine was not drained before shutdown returned")
		}
	})

	t.Run("onstart_and_onshutdown_hooks_run", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		srv := newLifecycleServer(t, nil, maniflex.Config{
			OnStart:    func(ctx context.Context) error { log.add("onstart"); return nil },
			OnShutdown: func(ctx context.Context) error { log.add("onshutdown"); return nil },
		})

		mustGet(t, srv.url+"/api/health", http.StatusOK)
		srv.cancel()
		<-srv.done

		want := []string{"onstart", "onshutdown"}
		if got := log.snapshot(); !equalStrings(got, want) {
			t.Errorf("hook order:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("onstart_error_aborts_boot", func(t *testing.T) {
		t.Parallel()
		log := &eventLog{}
		boom := errors.New("onstart boom")
		cfg := testServerConfig(t)
		cfg.OnStart = func(ctx context.Context) error { return boom }

		srv := maniflex.New(cfg)
		srv.MustRegister(testutil.DefaultModels()...)
		db, err := sqlite.Open(":memory:", srv.Registry())
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		srv.SetDB(db)

		// A service must never start when the OnStart hook fails first.
		srv.AddService(&recordingService{name: "a", log: log})

		err = srv.StartWithContext(context.Background())
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("StartWithContext: got %v, want wrapping %v", err, boom)
		}
		if got := log.snapshot(); len(got) != 0 {
			t.Errorf("services ran despite OnStart failure: %v", got)
		}
	})
}

// ── Infrastructure ────────────────────────────────────────────────────────────

// lifecycleServer is a running server plus the channels to drive its shutdown.
type lifecycleServer struct {
	url    string
	server *maniflex.Server
	cancel context.CancelFunc
	done   <-chan error
}

// log asserts the ordered events appear (as a prefix-agnostic subsequence) in
// the shared log; used for the "must have happened by now" checks.
func (s *lifecycleServer) log(l *eventLog, want ...string) bool {
	return containsInOrder(l.snapshot(), want)
}

// testServerConfig builds a Config bound to a random free port.
func testServerConfig(t *testing.T) maniflex.Config {
	t.Helper()
	return maniflex.Config{
		Port:            freePort(t),
		PathPrefix:      "/api",
		AutoMigrate:     true,
		ShutdownTimeout: 5 * time.Second,
	}
}

// newLifecycleServer starts a server on a random port, applying setup (which
// may register services / Server.Go) before StartWithContext, and merging the
// supplied hooks into the config.
func newLifecycleServer(t *testing.T, setup func(*maniflex.Server), cfg maniflex.Config) *lifecycleServer {
	t.Helper()

	port := freePort(t)
	cfg.Port = port
	if cfg.PathPrefix == "" {
		cfg.PathPrefix = "/api"
	}
	cfg.AutoMigrate = true
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}

	server := maniflex.New(cfg)
	server.MustRegister(testutil.DefaultModels()...)
	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)

	if setup != nil {
		setup(server)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- server.StartWithContext(ctx) }()

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

	t.Cleanup(cancel)
	return &lifecycleServer{url: baseURL, server: server, cancel: cancel, done: ch}
}

// freePort grabs a free TCP port on loopback and releases it for re-binding.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, portStr, _ := net.SplitHostPort(addr)
	port := 0
	fmt.Sscan(portStr, &port)
	return port
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsInOrder reports whether want appears as an ordered subsequence of got.
func containsInOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
