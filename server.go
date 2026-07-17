package maniflex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// loggerSetter is an optional interface that DB adapters may implement to
// receive the framework logger. sqlcore.Adapter implements this; the Server
// calls it automatically before AutoMigrate so migration logs go to the
// same sink as the rest of the framework output.
type loggerSetter interface {
	SetLogger(*slog.Logger)
}

// Server is the top-level server.
//
// Typical usage — signals handled automatically:
//
//	server := maniflex.New(maniflex.Config{DB: myAdapter})
//	server.MustRegister(User{}, Post{}, Comment{})
//	server.Pipeline.Auth.Register(jwtMiddleware)
//	if err := server.Start(); err != nil {
//	    log.Fatal(err)
//	}
//
// Advanced usage — caller controls the shutdown context:
//
//	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
//	defer stop()
//	if err := server.StartWithContext(ctx); err != nil {
//	    log.Fatal(err)
//	}
type Server struct {
	cfg          Config
	registry     *Registry
	steps        *defaultSteps    // held so SetDB can patch the adapter after construction
	oasSteps     *oasDefaultSteps // held so cfg pointer stays live for OpenAPI generation
	Pipeline     *Pipeline
	actions      []ActionConfig
	asyncCfg     *AsyncAPIConfig     // non-nil → mount /asyncapi.json (set via RealtimeDoc)
	globalSearch *GlobalSearchConfig // non-nil → mount /search (set via EnableGlobalSearch)
	rollups      []compiledRollup    // maintained aggregate columns (set via RegisterRollup)
	lifecycle    *lifecycle          // supervised services + Server.Go goroutines

	// mu guards the four fields below — the only Server state mutated after
	// construction, and the only state three different goroutines reach for:
	// Start (conventionally its own), Handler (a test's, an embedding mux's), and
	// Shutdown (a signal handler's). Registration is single-threaded by contract
	// and needs no lock.
	mu       sync.Mutex
	router   http.Handler // built exactly once, on the first Handler() or Start()
	httpSrv  *http.Server // published by StartWithContext just before the listener opens
	started  bool         // StartWithContext has been entered
	stopping bool         // Shutdown has been called — the listener must not open

	exited   chan struct{} // closed when StartWithContext returns; see markExited
	exitOnce sync.Once
}

// New creates a Server with the given configuration.
// Sensible defaults are applied for any zero-value fields.
func New(cfg Config) *Server {
	cfg.ApplyDefaults()

	reg := newRegistry()
	steps := newDefaultSteps(cfg.DB, reg)
	steps.storage = cfg.FilesConfig.Storage
	steps.keyProvider = cfg.KeyProvider
	steps.keyScope = cfg.FilesConfig.KeyScope
	steps.signedURLTTL = cfg.FilesConfig.SignedURLTTL
	steps.maxUpload = cfg.FilesConfig.MaxUploadBytes
	steps.maxUploadMem = cfg.FilesConfig.MaxUploadMemory

	srv := &Server{
		cfg:       cfg,
		registry:  reg,
		steps:     steps,
		lifecycle: newLifecycle(),
		exited:    make(chan struct{}),
	}

	// The OpenAPI generator must read the Config the server actually serves from —
	// &srv.cfg, not the constructor's copy. Pointing it at the local meant the
	// two-step init (New, then SetStorage/SetDB) updated only the server's copy,
	// so the spec omitted the file and attachment routes the router had mounted
	// (BUG-10). The router and handlers already take &srv.cfg for the same reason.
	srv.oasSteps = newOASDefaultSteps(reg, &srv.cfg)
	srv.Pipeline = newPipeline(steps, srv.oasSteps)
	return srv
}

// Register adds one or more models to the Server.
// It accepts any of:
//
//	server.Register(User{})
//	server.Register(User{}, ModelConfig{TableName: "members"})
//	server.Register(User{}, Post{}, Comment{})
//	server.Register([]any{User{}, Post{}})
func (c *Server) Register(args ...any) error {
	models, configs := flattenArgs(args)
	for i, v := range models {
		cfg := ModelConfig{}
		if i < len(configs) {
			cfg = configs[i]
		}
		meta, err := ScanModel(v, cfg)
		if err != nil {
			return err
		}
		if err := c.registry.add(meta); err != nil {
			return err
		}
		if meta.Config.Versioned {
			if err := c.registerVersioningFor(meta); err != nil {
				return fmt.Errorf("versioning setup for %s: %w", meta.Name, err)
			}
		}
		// Register any per-model middleware supplied in ModelConfig.Middleware.
		if m := cfg.Middleware; m != nil {
			for _, mw := range m.Auth {
				c.Pipeline.Auth.Register(mw, ForModel(meta.Name))
			}
			for _, mw := range m.Deserialize {
				c.Pipeline.Deserialize.Register(mw, ForModel(meta.Name))
			}
			for _, mw := range m.Validate {
				c.Pipeline.Validate.Register(mw, ForModel(meta.Name))
			}
			for _, mw := range m.Service {
				c.Pipeline.Service.Register(mw, ForModel(meta.Name))
			}
			for _, mw := range m.DB {
				c.Pipeline.DB.Register(mw, ForModel(meta.Name))
			}
			for _, mw := range m.Response {
				c.Pipeline.Response.Register(mw, ForModel(meta.Name))
			}
		}
	}
	return nil
}

// MustRegister calls Register and panics on error.
// Intended for use in package-level init or main().
func (c *Server) MustRegister(args ...any) {
	if err := c.Register(args...); err != nil {
		panic(fmt.Sprintf("maniflex: Register failed: %v", err))
	}
}

// AddService registers a long-lived background Service supervised by the
// server. Services start after migration and DB-ready, in registration order,
// before the HTTP listener opens, and stop in reverse order during graceful
// shutdown (before the background-goroutine drain). A Start error aborts boot.
//
// Must be called before Start/StartWithContext.
//
//	server.AddService(pool)                          // a custom Service
//	server.AddService(maniflex.ServiceFunc(startFn)) // adapter for a bare func
func (c *Server) AddService(s Service) {
	if c.hasStarted() {
		panic("maniflex: AddService must be called before Start()")
	}
	c.lifecycle.add(s)
}

// hasStarted reports whether StartWithContext has been entered. It is the honest
// version of the "too late to register" guard: the sentinel it replaces was
// `httpSrv != nil`, a field set only once migration and every service had already
// come up, so a call landing anywhere inside the boot window sailed past the guard
// and was then quietly ignored — a service added there is never started (DX-4).
func (c *Server) hasStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// sealed reports whether the routing table is fixed — the router has been built,
// or a boot that will build it is under way. Registration calls that mount a route
// (Action, RealtimeDoc, EnableGlobalSearch) panic once it is true, since anything
// they add past this point would silently never be served.
func (c *Server) sealed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started || c.router != nil
}

// Go runs fn on an application-scoped goroutine that the server drains during
// graceful shutdown — the same lifecycle as ctx.GoBackground, but tied to the
// server rather than a single request. The ctx passed to fn is cancelled when
// shutdown begins, so a long-running loop (e.g. a periodic reconciler) returns
// on ctx.Done() and is then waited on, bounded by Config.ShutdownTimeout.
//
// fn starts immediately, not at Start, and is drained however the server ends —
// including a boot that fails (a migration error, a service that would not start,
// a port already in use), so it is never abandoned mid-write.
//
//	server.Go(func(ctx context.Context) {
//	    t := time.NewTicker(time.Minute)
//	    defer t.Stop()
//	    for {
//	        select {
//	        case <-ctx.Done():
//	            return
//	        case <-t.C:
//	            reconcile(ctx)
//	        }
//	    }
//	})
func (c *Server) Go(fn func(context.Context)) {
	c.lifecycle.goRoutine(fn)
}

// Start performs auto-migration (if enabled), starts the HTTP server, and
// listens for SIGINT or SIGTERM. When a signal arrives it initiates a graceful
// shutdown: in-flight requests are given up to Config.ShutdownTimeout to
// complete before the server closes. Start returns nil if shutdown completes
// cleanly, or an error if the shutdown context expires or startup fails.
//
// Start is the correct entry point for production. It is equivalent to:
//
//	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
//	defer stop()
//	server.StartWithContext(ctx)
func (c *Server) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.StartWithContext(ctx)
}

// MigrateOnly runs auto-migration (if enabled in Config) and returns without
// starting the HTTP server. Use this for the Kubernetes init-container pattern
// where migrations run as a separate, single-shot process before the main
// server replicas start serving traffic:
//
//	if os.Getenv("MIGRATE_ONLY") == "1" {
//	    log.Fatal(server.MigrateOnly(ctx))
//	}
//	log.Fatal(server.Start())
func (c *Server) MigrateOnly(ctx context.Context) error {
	return c.migrate(ctx)
}

// migrate validates DB adapters, wires the framework logger into each, and
// runs AutoMigrate when enabled. Shared by Start* and MigrateOnly.
//
// With per-model adapter routing (ModelConfig.Adapter), models can be split
// across multiple DBs. Each distinct adapter is called once with a filtered
// registry view exposing only the models routed to it. Config.DB serves the
// models with no override; it may be nil when every model has its own adapter.
func (c *Server) migrate(ctx context.Context) error {
	groups, err := c.adapterGroups()
	if err != nil {
		return err
	}
	// No groups means no models are registered yet — there is nothing to
	// migrate or wire. This is a valid state: the smallest possible app only
	// serves /health and an empty OpenAPI document. A genuine missing-adapter
	// misconfiguration (models registered with no Config.DB and no per-model
	// adapter) is caught inside adapterGroups with a message naming the
	// offending models, so reaching here with zero groups is benign.
	if len(groups) == 0 {
		return nil
	}

	logger := c.cfg.logger()
	for _, g := range groups {
		if ls, ok := g.adapter.(loggerSetter); ok {
			ls.SetLogger(logger)
		}
	}

	if c.cfg.DisableAutoMigrate {
		return nil
	}

	for _, g := range groups {
		logger.Info("[maniflex] running auto-migration",
			slog.Int("models", len(g.models)))
		view := newFilteredRegistry(c.registry, g.models)
		if err := g.adapter.AutoMigrate(ctx, view); err != nil {
			return fmt.Errorf("maniflex: auto-migration failed: %w", err)
		}
	}
	logger.Info("[maniflex] migration complete")
	return nil
}

// adapterGroup pairs an adapter with the set of model names routed to it.
type adapterGroup struct {
	adapter DBAdapter
	models  []string // model names, in registration order
}

// adapterGroups groups registered models by their resolved adapter.
// Models with no per-model override resolve to Config.DB. An entry is omitted
// when its adapter is nil — so if Config.DB is nil and every model has its own
// adapter, only the per-model groups are returned.
//
// Returns an error if any model resolves to a nil adapter (no global and no
// per-model override) since that model cannot serve requests.
func (c *Server) adapterGroups() ([]adapterGroup, error) {
	models := c.registry.All()
	// Pointer-identity grouping; preserve discovery order so the first model
	// encountered defines the slot ordering.
	type slot struct {
		adapter DBAdapter
		models  []string
	}
	order := []DBAdapter{}
	byPtr := map[DBAdapter]*slot{}
	put := func(a DBAdapter, name string) {
		if s, ok := byPtr[a]; ok {
			s.models = append(s.models, name)
			return
		}
		s := &slot{adapter: a, models: []string{name}}
		byPtr[a] = s
		order = append(order, a)
	}

	var unrouted []string
	for _, m := range models {
		if m.Adapter != nil {
			put(m.Adapter, m.Name)
			continue
		}
		unrouted = append(unrouted, m.Name)
	}
	if len(unrouted) > 0 {
		if c.cfg.DB == nil {
			return nil, fmt.Errorf(
				"maniflex: no database adapter configured for model(s) %v "+
					"(set Config.DB or ModelConfig.Adapter)", unrouted)
		}
		for _, name := range unrouted {
			put(c.cfg.DB, name)
		}
	}

	out := make([]adapterGroup, 0, len(order))
	for _, a := range order {
		s := byPtr[a]
		out = append(out, adapterGroup{adapter: s.adapter, models: s.models})
	}
	return out, nil
}

// StartWithContext is like Start but uses the provided context to drive
// shutdown instead of OS signals. The server runs until ctx is cancelled, then
// performs a graceful shutdown with a fresh timeout context derived from
// Config.ShutdownTimeout.
//
// This variant is useful when:
//   - The caller manages its own signal handling.
//   - The server must shut down in response to application-level events.
//   - Tests need to stop the server without sending a real OS signal.
//
// Example:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
//	defer cancel()
//	server.StartWithContext(ctx) // server stops after 5 minutes
func (c *Server) StartWithContext(ctx context.Context) error {
	c.markStarted()
	defer c.markExited()

	if err := c.migrate(ctx); err != nil {
		c.abortGoroutines()
		return err
	}

	// Boot the application lifecycle: OnStart hook then supervised services, in
	// registration order. A failure here aborts boot like a failed migration —
	// services that already started are rolled back inside lifecycle.start.
	if err := c.lifecycle.start(&c.cfg); err != nil {
		c.abortGoroutines()
		return err
	}

	// Build the router before taking the lock — Handler takes it too.
	addr := fmt.Sprintf(":%d", c.cfg.Port)
	srv, ok := c.publishListener(addr, c.Handler())
	if !ok {
		// A Shutdown landed while we were migrating or starting services. It has
		// countermanded the boot rather than racing it, so open no listener; the
		// teardown is ours to run, since a Shutdown that stopped services from the
		// outside could do so underneath a lifecycle.start still bringing them up.
		c.cfg.logger().Info("[maniflex] shutdown requested during boot — the listener was never opened")
		stopCtx, stopCancel := context.WithTimeout(context.Background(), c.cfg.ShutdownTimeout)
		c.stopAndDrain(stopCtx)
		stopCancel()
		return nil
	}

	// Start listening in a background goroutine so we can block on ctx here.
	serveErr := make(chan error, 1)
	go func() {
		c.cfg.logger().Info("[maniflex] listening", slog.String("addr", addr), slog.String("prefix", c.cfg.PathPrefix))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// Block until the context is cancelled (signal received or caller cancelled)
	// or the server fails to start at all.
	select {
	case err := <-serveErr:
		if err != nil {
			// The server failed before we ever got a shutdown signal (port in
			// use, …). Boot did complete, so run the same teardown the graceful
			// path runs — services stopped, then Server.Go loops and in-flight
			// background writes drained — in a ShutdownTimeout window of its own,
			// since no drain is in progress to share one with. Stopping without
			// draining would cancel those goroutines and walk away mid-write,
			// which is the truncation the drain exists to prevent.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), c.cfg.ShutdownTimeout)
			c.stopAndDrain(stopCtx)
			stopCancel()
			return fmt.Errorf("maniflex: server error: %w", err)
		}
		// Closed without an error means Shutdown() was called explicitly while we
		// were serving. It owns the teardown and its budget — tearing down here
		// too would race it for the once-only lifecycle.stop and could hand the
		// services a fresh full-length window instead of the caller's deadline.
		return nil

	case <-ctx.Done():
		c.cfg.logger().Info("[maniflex] shutdown signal received — draining",
			slog.Duration("timeout", c.cfg.ShutdownTimeout))
	}

	// Give the whole graceful path a single bounded window.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), c.cfg.ShutdownTimeout)
	defer cancel()
	return c.gracefulShutdown(shutdownCtx)
}

// markStarted records that boot has begun, before it has done anything. Every
// "must be called before Start" guard reads this, and Shutdown reads it to tell a
// server that is booting from one that was never started at all.
func (c *Server) markStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
}

// markExited releases a Shutdown that is waiting on a boot to finish tearing
// itself down. It runs on every exit from StartWithContext, so the wait ends even
// when boot failed instead of countermanding.
func (c *Server) markExited() {
	c.exitOnce.Do(func() { close(c.exited) })
}

// publishListener installs the http.Server under the same lock Shutdown reads it
// with, and reports false if a Shutdown got there first.
//
// The two must be ordered against each other or the listener escapes: Shutdown
// read httpSrv with no lock, found the nil it holds for the whole boot window,
// concluded the server was not running and returned — while boot carried on behind
// it and opened the socket. The common `go srv.Start(); …; srv.Shutdown(ctx)` then
// left a bound port for the life of the process (DX-3).
func (c *Server) publishListener(addr string, h http.Handler) (*http.Server, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return nil, false
	}
	c.httpSrv = newHTTPServer(addr, h, &c.cfg)
	return c.httpSrv, true
}

// httpServer reads the published listener, nil until StartWithContext opens one.
func (c *Server) httpServer() *http.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.httpSrv
}

// newHTTPServer builds the http.Server the framework owns, carrying the timeouts
// from Config. net/http sets none of these by itself, which is what makes an
// out-of-the-box Go server answer slowloris by holding the connection open
// forever — so the framework, which owns this struct, supplies the defensive ones
// (see Config.ReadHeaderTimeout / IdleTimeout).
//
// A negative value in Config means "disable": it maps to the zero the http.Server
// reads as unbounded. That distinction is why the defaults live in ApplyDefaults
// (where zero still means "unset") rather than here.
func newHTTPServer(addr string, h http.Handler, cfg *Config) *http.Server {
	unbounded := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		return d
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: unbounded(cfg.ReadHeaderTimeout),
		IdleTimeout:       unbounded(cfg.IdleTimeout),
		ReadTimeout:       unbounded(cfg.ReadTimeout),
		WriteTimeout:      unbounded(cfg.WriteTimeout),
	}
}

// gracefulShutdown runs the ordered shutdown sequence, bounded by ctx:
// http.Shutdown (drain in-flight requests) → Service.Stop in reverse order +
// OnShutdown hook → drain Server.Go and ctx.GoBackground goroutines. It is the
// shared path behind both the signal/context route in StartWithContext and the
// explicit Shutdown method, so the order holds however shutdown is triggered.
func (c *Server) gracefulShutdown(ctx context.Context) error {
	var firstErr error

	// 1. Stop accepting new connections; let in-flight requests finish.
	if srv := c.httpServer(); srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			firstErr = fmt.Errorf("maniflex: graceful shutdown failed: %w", err)
		}
	}

	// 2-3. Stop the lifecycle and drain what it spawned, on the remaining budget.
	c.stopAndDrain(ctx)

	if firstErr == nil {
		c.cfg.logger().Info("[maniflex] shutdown complete")
	}
	return firstErr
}

// stopAndDrain is the non-HTTP half of shutdown, shared by the graceful path and
// by the boot-failure path in StartWithContext — a server that never got to serve
// still has to put down everything boot brought up.
//
// It cancels the lifecycle context (loops wind down), stops services in reverse
// order and runs the OnShutdown hook — all idempotent — and then drains the
// application-scoped Server.Go goroutines and the per-request ctx.GoBackground
// writes still in flight. Roadmap §11B.6: pre-fix those background writes used
// context.Background() and could be killed mid-write by the process exit; now
// they are tracked and waited on.
//
// Every phase runs on the caller's ctx — the one shutdown budget (BUG-11).
func (c *Server) stopAndDrain(ctx context.Context) {
	c.lifecycle.stop(ctx, &c.cfg)

	if !c.lifecycle.drain(ctx) {
		c.cfg.logger().Warn("[maniflex] shutdown: Server.Go goroutines did not complete")
	}
	if dropped := c.steps.bg.Wait(ctx); dropped > 0 {
		c.cfg.logger().Warn("[maniflex] shutdown: background writes did not complete",
			slog.Int64("in_flight", dropped))
	}
}

// abortGoroutines tears down after a boot that failed before any service came up
// — a failed migration, or a service whose Start returned an error (lifecycle.start
// has already rolled back the ones before it, in its own window).
//
// It cancels the lifecycle context and waits, but does not stop services or run
// the OnShutdown hook: nothing is running to stop, and a hook symmetric with an
// OnStart that failed (or never ran) is not owed. What it does reach are the
// Server.Go loops — those spawn the moment Go is called, not at Start, so a boot
// that aborts early would otherwise return with them still running, never even
// cancelled.
func (c *Server) abortGoroutines() {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.ShutdownTimeout)
	defer cancel()

	c.lifecycle.cancel()
	if !c.lifecycle.drain(ctx) {
		c.cfg.logger().Warn("[maniflex] boot aborted: Server.Go goroutines did not complete")
	}
	if dropped := c.steps.bg.Wait(ctx); dropped > 0 {
		c.cfg.logger().Warn("[maniflex] boot aborted: background writes did not complete",
			slog.Int64("in_flight", dropped))
	}
}

// Shutdown initiates a graceful shutdown of the running server and waits for
// in-flight requests AND tracked background goroutines (audit writes, cache
// invalidations) to complete, bounded by the provided context. If the server was
// never started, Shutdown is a no-op.
//
// It is safe to call from another goroutine at any point in the server's life,
// including while it is still booting — the usual shape in a test:
//
//	go func() { _ = server.Start() }()
//	...
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//	server.Shutdown(ctx)
//
// A Shutdown that arrives during boot (migration, or a service still starting)
// countermands it: the listener is never opened, Start unwinds what it had brought
// up and returns nil, and Shutdown waits for that to finish before returning.
//
// It is terminal, not a pause: once called, a Start that has not yet opened the
// listener will not open one, whether it is already booting or has yet to be
// called. A Server is not restartable.
func (c *Server) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	srv, booting := c.httpSrv, c.started
	c.stopping = true
	c.mu.Unlock()

	if srv != nil {
		return c.gracefulShutdown(ctx)
	}
	if !booting {
		return nil // never started — the documented no-op
	}
	// Started, but the listener is not up yet: boot is inside migration or service
	// start. Tearing down from here would race it — stopping services underneath a
	// lifecycle.start that is still bringing them up leaks whatever it starts after
	// us. The stopping flag we just set makes boot abort instead of listening and
	// unwind itself, so wait for it, on the caller's budget.
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler returns the underlying http.Handler without starting the server.
// Useful for testing or embedding into an existing HTTP mux.
//
// Unlike Start, Handler does NOT run auto-migration — call Start, MigrateOnly,
// or the adapter's AutoMigrate first, or requests will fail against missing tables.
//
// The router is built on the first call and reused thereafter. Concurrent callers
// block until it is built rather than each building one of their own: the build
// resolves many-to-many relations by writing back to the registry, so two of them
// at once raced over shared model metadata.
func (c *Server) Handler() http.Handler {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.router == nil {
		// Resolve many-to-many relations now that all models are registered.
		if err := resolveManyToMany(c.registry); err != nil {
			panic(fmt.Sprintf("maniflex: resolveManyToMany: %v", err))
		}
		// Validate lock_scope model references.
		if err := validateLockScopes(c.registry); err != nil {
			panic(err.Error())
		}
		// Warn about convention "<Name>ID" relations whose target model was never
		// registered (commonly a foreign id that should be mfx:"norelation").
		warnDanglingRelations(c.registry, c.cfg.logger())
		// Auto-register file cleanup middleware for models with file fields
		if c.cfg.FilesConfig.Storage != nil {
			c.registerFileCleanup()
		}
		// Warn about middleware registered on a pipeline step its operation can
		// never reach (e.g. ForOperation(OpSearch) on the Service step).
		warnIneffectiveMiddleware(c.Pipeline, c.cfg.logger())
		// Close the registration window — after this the composed chains are cached
		// per (model, operation) instead of rebuilt six times per request, so the
		// middleware set must stop changing. Last, because the file-cleanup hooks
		// above register middleware of their own (PERF-2).
		c.Pipeline.freeze()
		h := newHandlers(c.Pipeline, c.steps, &c.cfg)
		h.globalSearch = c.globalSearch
		c.router = buildRouter(&c.cfg, c.registry, h, c.Pipeline, c.cfg.logger(), c.actions, c.asyncCfg)
	}
	return c.router
}

// registerFileCleanup auto-registers Before-DB and After-DB middleware for
// every hard-delete model that has file fields with auto_delete enabled.
// Soft-delete models are skipped because the record (and its files) can be
// restored. The After-DB middleware deletes files asynchronously in a
// goroutine so it never blocks the HTTP response.
func (c *Server) registerFileCleanup() {
	for _, meta := range c.registry.All() {
		if !meta.HasFileFields() {
			continue
		}
		// Soft-delete models: files persist because the record can be restored.
		if meta.SoftDelete.Enabled {
			continue
		}

		// Collect fields that need auto-delete
		var autoDeleteFields []FieldMeta
		for _, f := range meta.FileFields() {
			if f.Tags.AutoDelete {
				autoDeleteFields = append(autoDeleteFields, f)
			}
		}
		if len(autoDeleteFields) == 0 {
			continue
		}

		modelName := meta.Name
		storage := c.cfg.FilesConfig.Storage

		// Before-DB: capture old file keys before the record is deleted
		captureOldKeys := func(ctx *ServerContext, next func() error) error {
			if ctx.ResourceID == "" {
				return next()
			}
			adapter := ctx.Model.ResolveAdapter(c.steps.adapter)
			if adapter == nil {
				return next()
			}
			existingRec, err := adapter.FindByID(ctx.Ctx, ctx.Model, ctx.ResourceID,
				&QueryParams{Limit: 1, Page: 1})
			if err != nil {
				return next() // can't fetch — skip cleanup, not critical
			}
			existing := recordToMap(ctx.Model, existingRec)
			// A flat key list, not one key per field: a maniflex.FileKeys column
			// holds many, so keying by column name could only ever carry the last
			// of them and would leak the rest.
			var keys []string
			for _, ff := range autoDeleteFields {
				keys = append(keys, fileKeysOfColumn(existing[ff.Tags.DBName])...)
			}
			ctx.Set("__file_old_keys", keys)
			return next()
		}

		// After-DB: delete old files asynchronously
		deleteOldFiles := func(ctx *ServerContext, next func() error) error {
			if err := next(); err != nil {
				return err
			}
			// Only proceed if DB delete succeeded
			if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
				return nil
			}
			oldKeys, _ := ctx.Get("__file_old_keys")
			if keys, ok := oldKeys.([]string); ok {
				ctx.GoBackground(func(bgCtx context.Context) {
					for _, key := range keys {
						if key != "" {
							// ErrFileNotFound is not a failure here — the file
							// was already gone (concurrent cleanup, manual
							// removal). Only log unexpected errors.
							if err := storage.Delete(bgCtx, key); err != nil && !errors.Is(err, ErrFileNotFound) {
								ctx.Logger().Warn("file-cleanup: storage delete failed",
									slog.String("key", key),
									slog.String("error", err.Error()))
							}
						}
					}
				})
			}
			return nil
		}

		c.Pipeline.DB.Register(captureOldKeys,
			ForModel(modelName),
			ForOperation(OpDelete),
			AtPosition(Before),
			WithName("file-cleanup-capture"),
		)
		c.Pipeline.DB.Register(deleteOldFiles,
			ForModel(modelName),
			ForOperation(OpDelete),
			AtPosition(After),
			WithName("file-cleanup-delete"),
		)
	}
}

// Registry returns the read-only model registry.
func (c *Server) Registry() RegistryAccessor {
	return c.registry
}

// PathPrefix returns the configured route prefix (e.g. "/api"). Satellite
// packages such as maniflex/admin use it to address the generated API in-process.
func (c *Server) PathPrefix() string {
	return c.cfg.PathPrefix
}

// DB returns the configured database adapter. It is used by packages such as
// jobs/maniflex that need to write directly to the database outside the HTTP
// pipeline (e.g. StatusSink.Transition called from a background worker).
func (c *Server) DB() DBAdapter {
	return c.cfg.DB
}

// SetStorage injects or replaces the file storage backend after construction.
// This allows the two-step init pattern (analogous to SetDB):
//
//	server := maniflex.New(maniflex.Config{...})
//	fs, _ := storage.NewLocalStorage("./uploads")
//	server.SetStorage(fs)
func (c *Server) SetStorage(fs FileStorage) {
	c.cfg.FilesConfig.Storage = fs
	c.steps.storage = fs
}

// SetKeyProvider injects or replaces the KeyProvider after construction.
// This allows the two-step init pattern:
//
//	server := maniflex.New(maniflex.Config{...})
//	server.SetKeyProvider(&encryption.EnvKeyProvider{Prefix: "MYAPP_KEY"})
func (c *Server) SetKeyProvider(kp KeyProvider) {
	c.cfg.KeyProvider = kp
	c.steps.keyProvider = kp
}

// KeyProvider returns the configured KeyProvider (nil if none). Use it to wire a
// background ServerContext for typed access to encrypted models:
//
//	bg := maniflex.NewBackground(ctx, srv.DB(), srv.Registry())
//	bg.SetKeyProvider(srv.KeyProvider())
func (c *Server) KeyProvider() KeyProvider { return c.cfg.KeyProvider }

// SetDB injects or replaces the database adapter after construction.
// This allows the two-step init pattern:
//
//	server := maniflex.New(maniflex.Config{...})
//	server.MustRegister(User{}, Post{})
//	db, _ := sqlite.Open(":memory:", server.Registry())
//	server.SetDB(db)
//	server.Start()
func (c *Server) SetDB(db DBAdapter) {
	c.cfg.DB = db
	c.steps.adapter = db
}

// Action registers a custom HTTP endpoint that participates in the Auth and
// Response pipeline steps. Deserialize, Validate, Service, and DB steps are
// skipped; the handler is responsible for body parsing (ctx.BindJSON) and
// setting ctx.Response or calling ctx.Abort.
//
// Must be called before Start() or Handler(). Panics if the server has
// already started or if the method+path conflicts with a registered model route.
func (c *Server) Action(cfg ActionConfig) {
	if c.sealed() {
		panic("maniflex: Action() must be called before Start() or Handler()")
	}
	// Fail loudly at registration rather than deferring a nil-handler panic to
	// the first real request, where the stack trace is far from this call.
	if cfg.Handler == nil {
		panic("maniflex: ActionConfig.Handler must not be nil")
	}
	if cfg.Method == "" || cfg.Path == "" {
		panic("maniflex: ActionConfig.Method and Path must not be empty")
	}
	c.checkActionConflict(cfg)
	c.actions = append(c.actions, cfg)
	c.oasSteps.actions = append(c.oasSteps.actions, cfg)
}

// RealtimeDoc enables the {PathPrefix}/asyncapi.json endpoint, which serves an
// AsyncAPI 2.6 document describing the realtime event channels clients can
// subscribe to over the realtime hub (see the realtime package). Declare custom
// event payloads via cfg.Events and/or set cfg.AutoModelEvents to derive
// <model>.created|updated|deleted channels from the registry.
//
// Must be called before Start() or Handler(). Apps that never call it gain no
// new endpoint.
func (c *Server) RealtimeDoc(cfg AsyncAPIConfig) {
	if c.sealed() {
		panic("maniflex: RealtimeDoc() must be called before Start() or Handler()")
	}
	c.asyncCfg = &cfg
}

// GlobalSearchConfig configures the built-in cross-model search endpoint. The
// zero value is valid — every field falls back to a default.
type GlobalSearchConfig struct {
	// Path is the route mounted under PathPrefix. Defaults to "/search".
	Path string
	// DefaultLimit is the merged-result cap when the request omits ?limit=.
	// Defaults to 20.
	DefaultLimit int
	// MaxLimit clamps ?limit= so a client cannot request an unbounded scan.
	// Defaults to 100; set negative to disable the clamp.
	MaxLimit int
}

// EnableGlobalSearch mounts the built-in cross-model search endpoint at
// {PathPrefix}{Path} (default GET /search). It fans the native full-text search
// out over every model with ModelConfig.GlobalSearchable set and merges the hits
// into one relevance-ranked {"data": [{model, id, snippet, score}, ...]} list.
//
// The endpoint runs only the global Auth pipeline step (and Response) — it does
// NOT apply per-model auth/tenancy middleware — so only opt models into
// GlobalSearchable that are safe to search this way, and gate the endpoint with
// Pipeline.Auth middleware (globally or ForOperation(OpSearch)). For a scoped
// search with the app's own authorisation, build a custom Action that calls
// ctx.Search with an explicit model list instead.
//
// Must be called before Start() or Handler(). Apps that never call it gain no
// new endpoint.
func (c *Server) EnableGlobalSearch(cfg ...GlobalSearchConfig) {
	if c.sealed() {
		panic("maniflex: EnableGlobalSearch() must be called before Start() or Handler()")
	}
	resolved := GlobalSearchConfig{}
	if len(cfg) > 0 {
		resolved = cfg[0]
	}
	if resolved.Path == "" {
		resolved.Path = "/search"
	}
	if !strings.HasPrefix(resolved.Path, "/") {
		resolved.Path = "/" + resolved.Path
	}
	if resolved.DefaultLimit <= 0 {
		resolved.DefaultLimit = defaultSearchLimit
	}
	if resolved.MaxLimit == 0 {
		resolved.MaxLimit = 100
	}
	c.globalSearch = &resolved
	c.oasSteps.globalSearch = &resolved
}

// routeShape is one auto-generated route: a human-readable kind (for the panic
// message), the URL split into segments (a path parameter is kept as "{name}"),
// and the HTTP methods mounted on it.
type routeShape struct {
	kind     string
	segments []string
	methods  []string
}

// modelRouteShapes returns the routes mountModel actually mounts for meta, so the
// conflict check sees the same surface the router does. The old check knew only
// the five CRUD routes, so an action shadowing /{model}/export, /{model}/aggregate,
// an attachment path, or a singleton's PATCH sailed past it — and because each
// model is mounted under its own sub-tree, the collision does not panic when the
// router builds: it silently shadows, and the model's endpoint quietly stops
// answering with no error anywhere (DX-7).
//
// Attachment routes are reserved whenever the model declares file fields, even
// though they mount only once storage is configured: SetStorage may run after
// Action(), so the path is claimed defensively rather than raced.
func modelRouteShapes(meta *ModelMeta) []routeShape {
	base := strings.TrimPrefix(meta.TableName, TABLE_NAME_PREFIX)

	if meta.Config.Singleton {
		// One row on the bare path: GET/PATCH, no id subtree, no POST/DELETE/list.
		return []routeShape{{"singleton", []string{base}, []string{"GET", "PATCH", "HEAD", "OPTIONS"}}}
	}

	shapes := []routeShape{
		{"collection", []string{base}, []string{"GET", "POST", "HEAD", "OPTIONS"}},
		{"item", []string{base, "{id}"}, []string{"GET", "PATCH", "DELETE", "HEAD", "OPTIONS"}},
	}
	if meta.Config.ExportEnabled {
		shapes = append(shapes, routeShape{"export", []string{base, "export"}, []string{"GET"}})
	}
	if meta.Config.AggregateEnabled {
		shapes = append(shapes, routeShape{"aggregate", []string{base, "aggregate"}, []string{"GET"}})
	}
	for _, ff := range meta.FileFields() {
		shapes = append(shapes, routeShape{"attachment", []string{base, "{id}", ff.Tags.JSONName}, []string{"GET"}})
		if ff.Tags.PresignedUpload {
			shapes = append(shapes, routeShape{
				"presigned upload",
				[]string{base, ff.Tags.JSONName, "upload-url"},
				[]string{"POST"},
			})
		}
	}
	return shapes
}

// pathSegments splits a URL path into its non-empty segments, so "/users/{id}/"
// and "/users/{id}" both yield [users {id}].
func pathSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// segIsWildcard reports whether a path segment is a chi parameter ("{id}") or the
// catch-all ("*") rather than a literal.
func segIsWildcard(s string) bool {
	return s == "*" || (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"))
}

// actionShadowsRoute reports whether an action at (method, segs) would resolve the
// same requests as the model route `shape` and so silently shadow it. That needs
// the same path structure — identical length, literals equal, and a parameter
// aligned with a parameter (its name is irrelevant: the router keys on position,
// not name) — AND a shared method. A parameter opposite a literal addresses a
// different route; so do the model's other methods, since each model is mounted
// under its own sub-tree and the router dispatches the untaken methods there. Both
// facts were verified against the router's actual request routing, not assumed.
func actionShadowsRoute(method string, segs []string, shape routeShape) bool {
	if len(segs) != len(shape.segments) {
		return false
	}
	for i := range segs {
		a, m := segs[i], shape.segments[i]
		if segIsWildcard(a) != segIsWildcard(m) {
			return false // a parameter and a literal address different routes
		}
		if !segIsWildcard(a) && a != m {
			return false // divergent literal — a different branch entirely
		}
	}
	return slices.Contains(shape.methods, method)
}

// checkActionConflict panics if cfg.Method+cfg.Path collides with another action
// or would shadow any route an auto-generated model mounts.
func (c *Server) checkActionConflict(cfg ActionConfig) {
	method := strings.ToUpper(cfg.Method)
	segs := pathSegments(cfg.Path)

	// Reject a second action with the same method+path. chi would mount both and
	// silently serve only the first, so surface it at registration instead.
	norm := strings.TrimSuffix(cfg.Path, "/")
	for _, existing := range c.actions {
		if strings.ToUpper(existing.Method) == method && strings.TrimSuffix(existing.Path, "/") == norm {
			panic(fmt.Sprintf("maniflex: duplicate action %s %s", method, cfg.Path))
		}
	}

	for _, meta := range c.registry.All() {
		// Headless models mount no REST routes, so they can't collide with an
		// action — that is the whole point of registering one behind an action.
		if meta.Config.Headless {
			continue
		}
		for _, shape := range modelRouteShapes(meta) {
			if actionShadowsRoute(method, segs, shape) {
				panic(fmt.Sprintf(
					"maniflex: action %s %s would shadow the auto-generated %s route for model %q",
					method, cfg.Path, shape.kind, meta.Name))
			}
		}
	}
}

// ── Register argument normalisation ──────────────────────────────────────────

// flattenArgs unwraps any combination of struct values, pointers,
// slices-of-any, and ModelConfig values into two parallel slices.
//
// Pairing rule: each ModelConfig is attached to the model that immediately
// precedes it in the argument list. The previous implementation zipped configs
// and models by their respective discovery indices, so a caller writing
// Register(User{}, Post{}, ModelConfig{B}) silently applied B to User (the
// first config matched the first model) instead of Post.
//
// flattenArgs logs warnings via slog.Default for two foot-gun shapes that
// indicate the caller misunderstood the pairing rule:
//   - a ModelConfig at position 0 (no preceding model to attach to)
//   - two ModelConfigs in a row (the second one has no fresh model to bind to)
//
// In both cases the offending config is dropped. A future Config.Strict mode
// (roadmap §10.1) will promote these warnings to panics.
func flattenArgs(args []any) (models []any, configs []ModelConfig) {
	prevWasConfig := false

	// add handles a single, already-unsliced argument: it binds a ModelConfig to
	// the preceding model, or records a struct / pointer-to-struct as a model.
	// Calling it for both top-level args and slice elements applies the
	// "ModelConfig follows its model" rule at any nesting depth — so the
	// idiomatic MustRegister(domainA.Models(), domainB.Models()) pattern, where
	// each Models() slice carries its own inline ModelConfigs, pairs them
	// correctly instead of routing a config struct into ScanModel (which panicked
	// with "model must embed BaseModel").
	add := func(arg any, pos int) {
		if arg == nil {
			return
		}

		// ModelConfig — bind to the previously added model.
		if cfg, ok := arg.(ModelConfig); ok {
			switch {
			case len(models) == 0:
				slog.Default().Warn("[maniflex] ignoring ModelConfig at argument position 0 — ModelConfig must follow a model",
					slog.Int("position", pos))
			case prevWasConfig:
				slog.Default().Warn("[maniflex] ignoring ModelConfig immediately after another ModelConfig — only one ModelConfig per model is honoured",
					slog.Int("position", pos))
			default:
				configs[len(models)-1] = cfg
			}
			prevWasConfig = true
			return
		}

		prevWasConfig = false

		// Struct or pointer-to-struct → a model.
		base := reflect.TypeOf(arg)
		for base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if base.Kind() == reflect.Struct {
			models = append(models, arg)
			configs = append(configs, ModelConfig{})
		}
	}

	for i, arg := range args {
		if arg == nil {
			continue
		}
		// Slice — flatten one level, handling each element as though it had been
		// passed as a top-level variadic arg, so inline ModelConfigs still bind
		// to the model element that precedes them.
		if v := reflect.ValueOf(arg); v.Kind() == reflect.Slice {
			for j := 0; j < v.Len(); j++ {
				add(v.Index(j).Interface(), i)
			}
			continue
		}
		add(arg, i)
	}
	return
}
