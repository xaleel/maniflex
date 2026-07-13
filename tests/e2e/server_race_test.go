package e2e

// The Server's two lazily-initialised fields — the router and the http.Server —
// were read and written across goroutines with no lock. Handler() checked
// `router == nil` and built one, so two concurrent callers each built their own
// (and each re-resolved many-to-many relations back into the shared registry);
// Shutdown() checked `httpSrv == nil` and no-op'd, which is exactly what it saw
// throughout the boot window, so a Shutdown racing a Start decided the server was
// not running while boot went on to open the listener behind it (DX-3).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

type routerRaceWidget struct {
	maniflex.BaseModel
	Name string `json:"name" mfx:"required"`
}

// slowBootService holds boot open the way a pool dialling its backend does — long
// enough for a Shutdown on another goroutine to land before the listener opens.
type slowBootService struct {
	entered chan struct{}
	release chan struct{}
	stopped atomic.Bool
}

func (s *slowBootService) Start(context.Context) error {
	close(s.entered)
	<-s.release
	return nil
}

func (s *slowBootService) Stop(context.Context) error {
	s.stopped.Store(true)
	return nil
}

func TestShutdownDuringBoot_NeverOpensTheListener(t *testing.T) {
	t.Parallel()

	// A port that is free and, unlike :0, knowable in advance — we have to be able
	// to prove nothing ever bound it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	svc := &slowBootService{entered: make(chan struct{}), release: make(chan struct{})}

	srv := maniflex.New(maniflex.Config{
		PathPrefix:         "/api",
		Port:               port,
		DisableAutoMigrate: true,
		ShutdownTimeout:    5 * time.Second,
	})
	srv.AddService(svc)

	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()

	started := make(chan error, 1)
	go func() { started <- srv.StartWithContext(startCtx) }()

	<-svc.entered // boot is inside the service; the listener is not up

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	time.Sleep(100 * time.Millisecond) // let Shutdown land while boot is parked
	close(svc.release)                 // boot resumes — and must now decline to listen

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}

	// Shutdown has returned, so the socket must be unbound and must stay that way:
	// boot has either declined to listen or is about to be caught doing it.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			conn.Close()
			t.Fatalf("%s is bound: Shutdown raced boot, saw no listener, and boot opened one anyway", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !svc.stopped.Load() {
		t.Error("the service boot had already started was never stopped")
	}

	startCancel()
	select {
	case err := <-started:
		if err != nil {
			t.Errorf("StartWithContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartWithContext never returned")
	}
}

func TestHandler_ConcurrentCallsBuildOneRouter(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(routerRaceWidget{})

	const n = 32
	routers := make([]http.Handler, n)
	release := make(chan struct{})
	var wg sync.WaitGroup

	for i := range n {
		wg.Go(func() {
			<-release
			routers[i] = srv.Handler()
		})
	}
	close(release)
	wg.Wait()

	for i, r := range routers {
		if r != routers[0] {
			t.Fatalf("goroutine %d got a router of its own: Handler() built more than one, "+
				"and each build resolves relations back into the shared registry", i)
		}
	}
}
