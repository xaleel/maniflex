package maniflex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type bug04Service struct {
	starts       atomic.Int32
	stops        atomic.Int32
	started      chan struct{}
	startRelease <-chan struct{}
	stopped      chan struct{}
}

func (s *bug04Service) Start(context.Context) error {
	s.starts.Add(1)
	if s.started != nil {
		close(s.started)
	}
	if s.startRelease != nil {
		<-s.startRelease
	}
	return nil
}

func (s *bug04Service) Stop(context.Context) error {
	s.stops.Add(1)
	if s.stopped != nil {
		close(s.stopped)
	}
	return nil
}

func bug04Server(cfg Config) *Server {
	cfg.PathPrefix = "/api"
	cfg.DisableAutoMigrate = true
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg)
}

func bug04BackgroundAction(
	srv *Server,
	started chan<- struct{},
	release <-chan struct{},
	finished chan<- struct{},
) {
	srv.Action(ActionConfig{
		Method: http.MethodGet,
		Path:   "/background",
		Handler: func(ctx *ServerContext) error {
			ctx.GoBackground(func(context.Context) {
				close(started)
				<-release
				close(finished)
			})
			ctx.Response = &APIResponse{StatusCode: http.StatusNoContent}
			return nil
		},
	})
}

func bug04Request(t *testing.T, srv *Server) {
	t.Helper()
	httpSrv := httptest.NewServer(srv.Handler())
	resp, err := http.Get(httpSrv.URL + "/api/background")
	if err != nil {
		httpSrv.Close()
		t.Fatalf("GET background action: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		httpSrv.Close()
		t.Fatalf("background action status = %d, want 204", resp.StatusCode)
	}
	// The embedding owns this listener and must drain it before asking Maniflex
	// to stop lifecycle work, so no later request can add another background job.
	httpSrv.Close()
}

func TestEmbeddedStartServicesAndShutdownOwnFrameworkLifecycle(t *testing.T) {
	var onStart, onShutdown atomic.Int32
	srv := bug04Server(Config{
		OnStart: func(context.Context) error {
			onStart.Add(1)
			return nil
		},
		OnShutdown: func(context.Context) error {
			onShutdown.Add(1)
			return nil
		},
	})

	serviceStopped := make(chan struct{})
	service := &bug04Service{stopped: serviceStopped}
	srv.AddService(service)

	goStarted := make(chan struct{})
	goFinished := make(chan struct{})
	srv.Go(func(ctx context.Context) {
		close(goStarted)
		<-ctx.Done()
		close(goFinished)
	})
	<-goStarted

	bgStarted := make(chan struct{})
	bgRelease := make(chan struct{})
	bgFinished := make(chan struct{})
	bug04BackgroundAction(srv, bgStarted, bgRelease, bgFinished)

	if err := srv.StartServices(); err != nil {
		t.Fatalf("StartServices: %v", err)
	}
	if err := srv.StartServices(); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second StartServices error = %v, want ErrAlreadyStarted", err)
	}
	if err := srv.StartWithContext(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("StartWithContext during embedded lifecycle error = %v, want ErrAlreadyStarted", err)
	}
	if service.starts.Load() != 1 || onStart.Load() != 1 {
		t.Fatalf("embedded start: service starts=%d OnStart=%d, want 1 each",
			service.starts.Load(), onStart.Load())
	}

	bug04Request(t, srv)
	<-bgStarted

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	<-serviceStopped
	<-goFinished
	if onShutdown.Load() != 1 {
		t.Fatalf("OnShutdown calls = %d, want 1", onShutdown.Load())
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before request background work drained: %v", err)
	default:
	}

	close(bgRelease)
	<-bgFinished
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if service.stops.Load() != 1 {
		t.Fatalf("service stops = %d, want 1", service.stops.Load())
	}
	if err := srv.StartWithContext(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("StartWithContext after embedded shutdown error = %v, want ErrStopped", err)
	}
}

func TestHandlerOnlyShutdownDrainsWorkWithoutStartingLifecycle(t *testing.T) {
	var onStart, onShutdown atomic.Int32
	srv := bug04Server(Config{
		OnStart: func(context.Context) error {
			onStart.Add(1)
			return nil
		},
		OnShutdown: func(context.Context) error {
			onShutdown.Add(1)
			return nil
		},
	})
	service := &bug04Service{}
	srv.AddService(service)

	goFinished := make(chan struct{})
	srv.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(goFinished)
	})

	bgStarted := make(chan struct{})
	bgRelease := make(chan struct{})
	bgFinished := make(chan struct{})
	bug04BackgroundAction(srv, bgStarted, bgRelease, bgFinished)
	bug04Request(t, srv)
	<-bgStarted

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()
	<-goFinished

	if service.starts.Load() != 0 || service.stops.Load() != 0 {
		t.Fatalf("Handler-only lifecycle touched service: starts=%d stops=%d",
			service.starts.Load(), service.stops.Load())
	}
	if onStart.Load() != 0 || onShutdown.Load() != 0 {
		t.Fatalf("Handler-only lifecycle ran hooks: OnStart=%d OnShutdown=%d",
			onStart.Load(), onShutdown.Load())
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before request background work drained: %v", err)
	default:
	}

	close(bgRelease)
	<-bgFinished
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := srv.StartServices(); !errors.Is(err, ErrStopped) {
		t.Fatalf("StartServices after Shutdown error = %v, want ErrStopped", err)
	}
}

func TestShutdownDuringEmbeddedServiceStartCountermandsStartup(t *testing.T) {
	startRelease := make(chan struct{})
	started := make(chan struct{})
	stopped := make(chan struct{})
	service := &bug04Service{
		started:      started,
		startRelease: startRelease,
		stopped:      stopped,
	}
	srv := bug04Server(Config{})
	srv.AddService(service)

	startDone := make(chan error, 1)
	go func() { startDone <- srv.StartServices() }()
	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		srv.mu.Lock()
		state := srv.state
		srv.mu.Unlock()
		if state == serverStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown did not countermand embedded startup")
		}
		runtime.Gosched()
	}
	close(startRelease)

	if err := <-startDone; err != nil {
		t.Fatalf("countermanded StartServices: %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown during StartServices: %v", err)
	}
	<-stopped
	if service.starts.Load() != 1 || service.stops.Load() != 1 {
		t.Fatalf("service starts=%d stops=%d, want 1 each",
			service.starts.Load(), service.stops.Load())
	}
}
