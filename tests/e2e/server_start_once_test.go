package e2e

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// startGateService holds the accepted boot in the starting state. starts counts
// every call, so the tests prove rejected StartWithContext calls never reach
// lifecycle side effects.
type startGateService struct {
	entered chan struct{}
	release chan struct{}
	starts  atomic.Int32
}

func newStartGateService() *startGateService {
	return &startGateService{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *startGateService) Start(context.Context) error {
	if s.starts.Add(1) == 1 {
		close(s.entered)
	}
	<-s.release
	return nil
}

func (*startGateService) Stop(context.Context) error { return nil }

func startOnceServer(t *testing.T, service maniflex.Service) *maniflex.Server {
	t.Helper()
	srv := maniflex.New(maniflex.Config{
		Port:               freePort(t),
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
		ShutdownTimeout:    2 * time.Second,
	})
	if service != nil {
		srv.AddService(service)
	}
	return srv
}

func TestStartWithContext_SequentialSecondStartIsRejected(t *testing.T) {
	gate := newStartGateService()
	srv := startOnceServer(t, gate)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- srv.StartWithContext(ctx) }()
	<-gate.entered

	second := make(chan error, 1)
	go func() { second <- srv.StartWithContext(context.Background()) }()
	select {
	case err := <-second:
		if !errors.Is(err, maniflex.ErrAlreadyStarted) {
			t.Fatalf("second StartWithContext error = %v, want ErrAlreadyStarted", err)
		}
	case <-time.After(time.Second):
		cancel()
		close(gate.release)
		t.Fatal("second StartWithContext did not reject an active start promptly")
	}

	cancel()
	close(gate.release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("accepted StartWithContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accepted StartWithContext did not finish")
	}

	if got := gate.starts.Load(); got != 1 {
		t.Errorf("service Start calls = %d, want 1", got)
	}
	if err := srv.StartWithContext(context.Background()); !errors.Is(err, maniflex.ErrStopped) {
		t.Errorf("start after clean shutdown error = %v, want ErrStopped", err)
	}
}

func TestStartWithContext_ConcurrentCallsOnlyOneBoots(t *testing.T) {
	const callers = 16

	gate := newStartGateService()
	srv := startOnceServer(t, gate)
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, callers)
	begin := make(chan struct{})
	for range callers {
		go func() {
			<-begin
			results <- srv.StartWithContext(ctx)
		}()
	}
	close(begin)
	<-gate.entered

	for i := 0; i < callers-1; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, maniflex.ErrAlreadyStarted) {
				t.Fatalf("concurrent loser %d error = %v, want ErrAlreadyStarted", i, err)
			}
		case <-time.After(2 * time.Second):
			cancel()
			close(gate.release)
			t.Fatal("concurrent StartWithContext calls did not reject promptly")
		}
	}

	cancel()
	close(gate.release)
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("winning StartWithContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winning StartWithContext did not finish")
	}
	if got := gate.starts.Load(); got != 1 {
		t.Errorf("service Start calls = %d, want 1", got)
	}
}

func TestStartWithContext_AfterFailureReturnsErrStopped(t *testing.T) {
	bootErr := errors.New("dependency unavailable")
	var starts atomic.Int32
	srv := startOnceServer(t, maniflex.ServiceFunc(func(context.Context) error {
		starts.Add(1)
		return bootErr
	}))

	if err := srv.StartWithContext(context.Background()); !errors.Is(err, bootErr) {
		t.Fatalf("first StartWithContext error = %v, want wrapped boot error", err)
	}
	if err := srv.StartWithContext(context.Background()); !errors.Is(err, maniflex.ErrStopped) {
		t.Errorf("start after failed boot error = %v, want ErrStopped", err)
	}
	if got := starts.Load(); got != 1 {
		t.Errorf("service Start calls = %d, want 1", got)
	}
}

func TestStartWithContext_AfterShutdownReturnsErrStopped(t *testing.T) {
	srv := startOnceServer(t, nil)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before start: %v", err)
	}
	if err := srv.StartWithContext(context.Background()); !errors.Is(err, maniflex.ErrStopped) {
		t.Errorf("StartWithContext after Shutdown error = %v, want ErrStopped", err)
	}
}
