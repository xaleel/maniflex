package maniflex

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Service is an application-owned, long-lived background component supervised
// by the Server: a poller, cache warmer, queue consumer, or an in-memory pool
// manager. Register one with Server.AddService and the framework wires it into
// the boot and shutdown lifecycle instead of the caller hand-supervising it
// around Start.
//
// Boot order:    migrate → OnStart → Service.Start (registration order) → listen.
// Shutdown order: http.Shutdown → Service.Stop (reverse order) → OnShutdown →
//
//	drain Server.Go + ctx.GoBackground goroutines.
//
// All of shutdown is bounded by Config.ShutdownTimeout.
type Service interface {
	// Start is called once, after migration and DB-ready, before the HTTP
	// listener opens. It must return promptly — spawn any long-running loop on
	// its own goroutine (or via Server.Go) rather than blocking here. The ctx
	// is cancelled when shutdown begins, so a loop that selects on ctx.Done()
	// winds itself down without waiting for Stop.
	//
	// A non-nil error aborts boot exactly like a failed migration; services that
	// already started are stopped in reverse order first.
	Start(ctx context.Context) error

	// Stop is called once during graceful shutdown, after the HTTP listener has
	// drained and after the Start ctx has been cancelled. Use the ctx to flush
	// buffers, close connections, or wait for in-flight work to settle.
	//
	// The ctx carries what remains of the one shutdown budget — Config.Shutdown-
	// Timeout, or the deadline passed to Server.Shutdown — shared with the HTTP
	// drain that ran before it and the OnShutdown hook and goroutine drain that
	// run after. A slow drain leaves Stop less time, so honour the ctx: it is
	// what keeps total shutdown inside the window an orchestrator gives you
	// before it sends SIGKILL.
	Stop(ctx context.Context) error
}

// ServiceFunc adapts a bare start function into a Service. Stop is a no-op, so
// the function is expected to honour cancellation of the ctx passed to Start
// (e.g. a loop that returns on ctx.Done()).
//
//	server.AddService(maniflex.ServiceFunc(func(ctx context.Context) error {
//	    go poller.Run(ctx) // exits when ctx is cancelled at shutdown
//	    return nil
//	}))
type ServiceFunc func(ctx context.Context) error

// Start runs the wrapped function.
func (f ServiceFunc) Start(ctx context.Context) error { return f(ctx) }

// Stop is a no-op; the function honours ctx cancellation from Start instead.
func (ServiceFunc) Stop(context.Context) error { return nil }

// lifecycle owns the supervised services, the application-scoped goroutines
// spawned by Server.Go, and the context that ties their cancellation to
// shutdown. It is created once in New() so Server.Go works before Start and
// after Handler-only embedding.
type lifecycle struct {
	services []Service

	ctx    context.Context    // cancelled when shutdown begins
	cancel context.CancelFunc // cancels ctx
	wg     sync.WaitGroup     // tracks Server.Go goroutines

	stopOnce sync.Once
}

func newLifecycle() *lifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifecycle{ctx: ctx, cancel: cancel}
}

// add registers a service to be supervised. Must be called before Start.
func (l *lifecycle) add(s Service) {
	l.services = append(l.services, s)
}

// goRoutine schedules fn on an application-scoped goroutine drained by stop.
// fn receives the lifecycle context, which is cancelled when shutdown begins
// so long-running loops exit on their own.
func (l *lifecycle) goRoutine(fn func(context.Context)) {
	l.wg.Go(func() { fn(l.ctx) })
}

// start runs the OnStart hook then every service's Start in registration order.
// On the first error it stops the services that already started (reverse order)
// and returns the wrapped error so the caller can abort boot.
func (l *lifecycle) start(cfg *Config) error {
	if cfg.OnStart != nil {
		if err := cfg.OnStart(l.ctx); err != nil {
			return fmt.Errorf("maniflex: OnStart hook failed: %w", err)
		}
	}
	for i, svc := range l.services {
		if err := svc.Start(l.ctx); err != nil {
			// Boot rollback, not shutdown: there is no shutdown budget to share,
			// so give the rollback its own ShutdownTimeout window.
			stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			l.stopServices(stopCtx, cfg, i) // roll back the ones that came up
			cancel()
			return fmt.Errorf("maniflex: service start failed: %w", err)
		}
	}
	return nil
}

// stopServices stops services[0:n] in reverse order, bounded by ctx. Used both
// for rollback during a failed boot and for orderly shutdown.
func (l *lifecycle) stopServices(ctx context.Context, cfg *Config, n int) {
	if n <= 0 {
		return
	}
	for i := n - 1; i >= 0; i-- {
		if err := l.services[i].Stop(ctx); err != nil {
			cfg.logger().Error("[maniflex] service stop failed",
				slog.Int("index", i), slog.String("error", err.Error()))
		}
	}
}

// stop performs the lifecycle half of graceful shutdown: cancel the shared
// context (so loops wind down), stop services in reverse order, then run the
// OnShutdown hook. It runs at most once; later calls are no-ops. Draining the
// Server.Go goroutines is left to drain so it can share the HTTP drain budget.
//
// Every phase runs on the caller's ctx — the one shutdown budget. Minting a
// fresh ShutdownTimeout here (as the service stop and the OnShutdown hook each
// used to) meant the phases were additive: an HTTP drain, a hung Service.Stop
// and a slow OnShutdown could take 3× ShutdownTimeout between them, and an
// explicit Shutdown(ctx) deadline was ignored outright (BUG-11).
func (l *lifecycle) stop(ctx context.Context, cfg *Config) {
	l.stopOnce.Do(func() {
		l.cancel()
		l.stopServices(ctx, cfg, len(l.services))
		if cfg.OnShutdown != nil {
			if err := cfg.OnShutdown(ctx); err != nil {
				cfg.logger().Error("[maniflex] OnShutdown hook failed",
					slog.String("error", err.Error()))
			}
		}
	})
}

// drain waits for the Server.Go goroutines to finish, bounded by ctx. Returns
// false if the deadline elapsed with goroutines still running.
func (l *lifecycle) drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
