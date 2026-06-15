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
	// drained and after the Start ctx has been cancelled. The ctx is a fresh
	// deadline context bounded by Config.ShutdownTimeout — use it to flush
	// buffers, close connections, or wait for in-flight work to settle.
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
			l.stopServices(cfg, i) // roll back the ones that came up
			return fmt.Errorf("maniflex: service start failed: %w", err)
		}
	}
	return nil
}

// stopServices stops services[0:n] in reverse order with a fresh deadline
// context bounded by ShutdownTimeout. Used both for rollback during a failed
// boot and for orderly shutdown.
func (l *lifecycle) stopServices(cfg *Config, n int) {
	if n <= 0 {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	for i := n - 1; i >= 0; i-- {
		if err := l.services[i].Stop(stopCtx); err != nil {
			cfg.logger().Error("[maniflex] service stop failed",
				slog.Int("index", i), slog.String("error", err.Error()))
		}
	}
}

// stop performs the lifecycle half of graceful shutdown: cancel the shared
// context (so loops wind down), stop services in reverse order, then run the
// OnShutdown hook. It runs at most once; later calls are no-ops. Draining the
// Server.Go goroutines is left to drain so it can share the HTTP drain budget.
func (l *lifecycle) stop(cfg *Config) {
	l.stopOnce.Do(func() {
		l.cancel()
		l.stopServices(cfg, len(l.services))
		if cfg.OnShutdown != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := cfg.OnShutdown(stopCtx); err != nil {
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
