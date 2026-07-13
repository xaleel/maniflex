package e2e

// "Must be called before Start()" was enforced with sentinels that only went up
// long after Start() had been called: AddService tested `httpSrv != nil`, a field
// set once migration and *every service* had already come up, and Action tested
// `router != nil`, built later still. A call landing anywhere inside the boot
// window — the seconds a migration or a service dialling its backend takes, and
// exactly when a concurrent goroutine would make one — sailed past the guard and
// was then quietly ignored: lifecycle.start has already ranged over the service
// slice, so the service is never started, and nothing says so (DX-4).

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// bootWindowServer returns a server parked inside its boot window — a service of
// its own has been entered and will not return until release is closed — together
// with the func that lets boot finish and tears the server down.
func bootWindowServer(t *testing.T) (srv *maniflex.Server, teardown func()) {
	t.Helper()

	blocker := &slowBootService{entered: make(chan struct{}), release: make(chan struct{})}
	srv = maniflex.New(maniflex.Config{
		PathPrefix:         "/api",
		Port:               freePort(t),
		DisableAutoMigrate: true,
		ShutdownTimeout:    5 * time.Second,
	})
	srv.AddService(blocker)

	startCtx, startCancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- srv.StartWithContext(startCtx) }()

	<-blocker.entered // boot is inside the service; nothing is listening yet

	return srv, func() {
		close(blocker.release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		startCancel()
		select {
		case err := <-started:
			if err != nil {
				t.Errorf("StartWithContext: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("StartWithContext never returned")
		}
	}
}

func TestAddServiceInsideTheBootWindow_IsRejected(t *testing.T) {
	t.Parallel()

	srv, teardown := bootWindowServer(t)

	var lateStarted atomic.Bool
	late := maniflex.ServiceFunc(func(context.Context) error {
		lateStarted.Store(true)
		return nil
	})

	panicked := func() (p bool) {
		defer func() { p = recover() != nil }()
		srv.AddService(late)
		return
	}()

	teardown() // let boot finish, then stop the server

	if !panicked {
		t.Fatalf("AddService was accepted inside the boot window (the service it added ran: %v). "+
			"The guard it passed only goes up once every service has already started, so the "+
			"service is silently never started — the one thing the guard exists to prevent.",
			lateStarted.Load())
	}
}

func TestActionInsideTheBootWindow_IsRejected(t *testing.T) {
	t.Parallel()

	srv, teardown := bootWindowServer(t)

	panicked := func() (p bool) {
		defer func() { p = recover() != nil }()
		srv.Action(maniflex.ActionConfig{
			Method:  http.MethodGet,
			Path:    "/late",
			Handler: func(*maniflex.ServerContext) error { return nil },
		})
		return
	}()

	teardown()

	if !panicked {
		t.Fatal("Action was accepted inside the boot window: the guard tests a router that " +
			"this very boot has not built yet, so it races the build instead of rejecting the call")
	}
}
