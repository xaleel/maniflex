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
	"strings"
	"syscall"
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
	router       http.Handler // built lazily on first call to Handler() or Start()
	httpSrv      *http.Server // set by Start/StartWithContext; nil until then
	actions      []ActionConfig
	asyncCfg     *AsyncAPIConfig     // non-nil → mount /asyncapi.json (set via RealtimeDoc)
	globalSearch *GlobalSearchConfig // non-nil → mount /search (set via EnableGlobalSearch)
	lifecycle    *lifecycle          // supervised services + Server.Go goroutines
}

// New creates a Server with the given configuration.
// Sensible defaults are applied for any zero-value fields.
func New(cfg Config) *Server {
	cfg.ApplyDefaults()

	reg := newRegistry()
	steps := newDefaultSteps(cfg.DB, reg)
	steps.storage = cfg.FilesConfig.Storage
	steps.keyProvider = cfg.KeyProvider
	steps.signedURLTTL = cfg.FilesConfig.SignedURLTTL
	steps.maxUpload = cfg.FilesConfig.MaxUploadBytes
	steps.maxUploadMem = cfg.FilesConfig.MaxUploadMemory

	srv := &Server{
		cfg:       cfg,
		registry:  reg,
		steps:     steps,
		lifecycle: newLifecycle(),
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
	if c.httpSrv != nil {
		panic("maniflex: AddService must be called before Start()")
	}
	c.lifecycle.add(s)
}

// Go runs fn on an application-scoped goroutine that the server drains during
// graceful shutdown — the same lifecycle as ctx.GoBackground, but tied to the
// server rather than a single request. The ctx passed to fn is cancelled when
// shutdown begins, so a long-running loop (e.g. a periodic reconciler) returns
// on ctx.Done() and is then waited on, bounded by Config.ShutdownTimeout.
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
	if err := c.migrate(ctx); err != nil {
		return err
	}

	// Boot the application lifecycle: OnStart hook then supervised services, in
	// registration order. A failure here aborts boot like a failed migration —
	// services that already started are rolled back inside lifecycle.start.
	if err := c.lifecycle.start(&c.cfg); err != nil {
		return err
	}

	addr := fmt.Sprintf(":%d", c.cfg.Port)
	c.httpSrv = &http.Server{
		Addr:    addr,
		Handler: c.Handler(),
	}

	// Start listening in a background goroutine so we can block on ctx here.
	serveErr := make(chan error, 1)
	go func() {
		c.cfg.logger().Info("[maniflex] listening", slog.String("addr", addr), slog.String("prefix", c.cfg.PathPrefix))
		if err := c.httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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
			// use, …). Tear the lifecycle down so already-started services stop
			// cleanly, in a ShutdownTimeout window of its own — no drain is in
			// progress to share one with.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), c.cfg.ShutdownTimeout)
			c.lifecycle.stop(stopCtx, &c.cfg)
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

// gracefulShutdown runs the ordered shutdown sequence, bounded by ctx:
// http.Shutdown (drain in-flight requests) → Service.Stop in reverse order +
// OnShutdown hook → drain Server.Go and ctx.GoBackground goroutines. It is the
// shared path behind both the signal/context route in StartWithContext and the
// explicit Shutdown method, so the order holds however shutdown is triggered.
func (c *Server) gracefulShutdown(ctx context.Context) error {
	var firstErr error

	// 1. Stop accepting new connections; let in-flight requests finish.
	if c.httpSrv != nil {
		if err := c.httpSrv.Shutdown(ctx); err != nil {
			firstErr = fmt.Errorf("maniflex: graceful shutdown failed: %w", err)
		}
	}

	// 2. Cancel the lifecycle context (loops wind down), stop services in
	//    reverse order, then run the OnShutdown hook. Idempotent. It runs on the
	//    same ctx as every other phase, so whatever the HTTP drain used comes out
	//    of the same budget rather than starting a fresh one (BUG-11).
	c.lifecycle.stop(ctx, &c.cfg)

	// 3. Drain application-scoped Server.Go goroutines and per-request
	//    ctx.GoBackground writes within the remaining budget. Roadmap §11B.6:
	//    pre-fix the background writes used context.Background() and could be
	//    killed mid-write by the process exit; now they are tracked and waited on.
	if !c.lifecycle.drain(ctx) {
		c.cfg.logger().Warn("[maniflex] shutdown: Server.Go goroutines did not complete")
	}
	if dropped := c.steps.bg.Wait(ctx); dropped > 0 {
		c.cfg.logger().Warn("[maniflex] shutdown: background writes did not complete",
			slog.Int64("in_flight", dropped))
	}

	if firstErr == nil {
		c.cfg.logger().Info("[maniflex] shutdown complete")
	}
	return firstErr
}

// Shutdown initiates a graceful shutdown of the running server and waits for
// in-flight requests AND tracked background goroutines (audit writes, cache
// invalidations) to complete, bounded by the provided context. If the server
// is not running, Shutdown is a no-op.
//
// Typical usage in tests or when embedding the server:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//	server.Shutdown(ctx)
func (c *Server) Shutdown(ctx context.Context) error {
	if c.httpSrv == nil {
		return nil
	}
	return c.gracefulShutdown(ctx)
}

// Handler returns the underlying http.Handler without starting the server.
// Useful for testing or embedding into an existing HTTP mux.
//
// Unlike Start, Handler does NOT run auto-migration — call Start, MigrateOnly,
// or the adapter's AutoMigrate first, or requests will fail against missing tables.
func (c *Server) Handler() http.Handler {
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
			keys := make(map[string]string)
			for _, ff := range autoDeleteFields {
				if v, ok := existing[ff.Tags.DBName].(string); ok && v != "" {
					keys[ff.Tags.DBName] = v
				}
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
			if keys, ok := oldKeys.(map[string]string); ok {
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
	if c.router != nil {
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
	if c.router != nil {
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
	if c.router != nil {
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

// checkActionConflict panics if cfg.Method+cfg.Path duplicates a model route.
func (c *Server) checkActionConflict(cfg ActionConfig) {
	method := strings.ToUpper(cfg.Method)
	norm := strings.TrimSuffix(cfg.Path, "/")

	// Reject a second action with the same method+path. chi would mount both and
	// silently serve only the first, so surface it at registration instead.
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
		base := "/" + strings.TrimPrefix(meta.TableName, TABLE_NAME_PREFIX)
		item := base + "/{id}"
		if norm == base && (method == "GET" || method == "POST" || method == "HEAD" || method == "OPTIONS") {
			panic(fmt.Sprintf("maniflex: action %s %s conflicts with auto-generated route for model %q", method, cfg.Path, meta.Name))
		}
		if norm == item && (method == "GET" || method == "PATCH" || method == "DELETE" || method == "HEAD" || method == "OPTIONS") {
			panic(fmt.Sprintf("maniflex: action %s %s conflicts with auto-generated route for model %q", method, cfg.Path, meta.Name))
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
