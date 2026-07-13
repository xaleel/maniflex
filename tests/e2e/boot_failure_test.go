package e2e

// A boot that fails still has to put down what boot brought up. StartWithContext
// returned straight from its failure paths: the ListenAndServe branch cancelled
// the Server.Go loops but never awaited them (and never waited on in-flight
// background writes), and the failed-migration / failed-service branches returned
// without so much as cancelling them — Server.Go spawns the moment Go is called,
// not at Start. Either way the goroutines were abandoned mid-work, which is the
// truncation the shutdown drain exists to prevent (BUG-20).

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// A model whose field has no SQL column mapping — AutoMigrate fails on it.
type unmappableWidget struct {
	maniflex.BaseModel
	Tags []string `json:"tags" db:"tags"`
}

// winddownLoop registers a Server.Go loop that, once cancelled, takes a moment to
// finish its work — as a reconciler flushing a last write would. It returns a
// flag reporting whether that work completed, and blocks until the loop is up.
func winddownLoop(t *testing.T, srv *maniflex.Server) *atomic.Bool {
	t.Helper()
	var finished atomic.Bool
	running := make(chan struct{})

	srv.Go(func(ctx context.Context) {
		close(running)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // still flushing
		finished.Store(true)
	})
	<-running
	return &finished
}

func TestBootFailure_PortInUse_DrainsGoroutines(t *testing.T) {
	t.Parallel()

	// Hold the port so ListenAndServe cannot have it.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()

	srv := maniflex.New(maniflex.Config{
		PathPrefix:         "/api",
		Port:               ln.Addr().(*net.TCPAddr).Port,
		DisableAutoMigrate: true,
		ShutdownTimeout:    2 * time.Second,
	})
	finished := winddownLoop(t, srv)

	if err := srv.StartWithContext(context.Background()); err == nil {
		t.Fatal("expected StartWithContext to fail: the port is taken")
	}
	if !finished.Load() {
		t.Error("StartWithContext returned while a Server.Go goroutine was still winding down")
	}
}

func TestBootFailure_MigrationFails_DrainsGoroutines(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{
		PathPrefix:      "/api",
		ShutdownTimeout: 2 * time.Second,
	})
	srv.MustRegister(unmappableWidget{})

	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	srv.SetDB(db)

	finished := winddownLoop(t, srv)

	if err := srv.StartWithContext(context.Background()); err == nil {
		t.Fatal("expected StartWithContext to fail: the model has an unmappable column")
	}
	if !finished.Load() {
		t.Error("StartWithContext returned while a Server.Go goroutine was still winding down")
	}
}

func TestBootFailure_ServiceStartFails_DrainsGoroutines(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
		ShutdownTimeout:    2 * time.Second,
	})
	srv.AddService(maniflex.ServiceFunc(func(context.Context) error {
		return errors.New("cache warmer could not reach its backend")
	}))

	finished := winddownLoop(t, srv)

	if err := srv.StartWithContext(context.Background()); err == nil {
		t.Fatal("expected StartWithContext to fail: the service refused to start")
	}
	if !finished.Load() {
		t.Error("StartWithContext returned while a Server.Go goroutine was still winding down")
	}
}
