package maniflex

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// buildRouter constructs the chi.Router with all auto-generated model routes
// mounted under cfg.PathPrefix, plus the /openapi.json spec endpoint.
func buildRouter(cfg *Config, reg *Registry, h *handlers, p *Pipeline, l *slog.Logger, actions []ActionConfig, asyncCfg *AsyncAPIConfig) chi.Router {
	r := chi.NewRouter()

	// PanicRecoverer replaces chi's built-in Recoverer. It catches panics,
	// logs them as structured JSON via cfg.PanicLogger (or slog.Default()),
	// and returns a {"error": {"code": "PANIC", ...}} response consistent
	// with every other maniflex error envelope.
	r.Use(PanicRecoverer(cfg.PanicLogger))
	r.Use(chiMiddleware.RequestID)

	// RealIP rewrites RemoteAddr from X-Forwarded-For / X-Real-IP. Only trust
	// those client-supplied headers when the operator has opted in (the server
	// sits behind a proxy that strips inbound XFF); otherwise a client could
	// spoof its IP and defeat per-IP rate limiting and poison audit logs (SEC-5).
	if cfg.TrustProxyHeaders {
		r.Use(chiMiddleware.RealIP)
	}

	r.Route(cfg.PathPrefix, func(r chi.Router) {
		// Health-check
		r.Get("/health", healthHandler(cfg, reg))

		// OpenAPI 3.1 spec — runs through its own three-step pipeline
		r.Get("/openapi.json", p.OpenAPI.handler())

		// AsyncAPI 2.6 event-channel document — mounted only when the app opted
		// in via Server.RealtimeDoc, so CRUD-only apps gain no new endpoint.
		if asyncCfg != nil {
			r.Get("/asyncapi.json", asyncAPIHandler(reg, cfg, *asyncCfg))
		}

		// Built-in cross-model search (4.10) — mounted only when the app opted in
		// via Server.EnableGlobalSearch. Runs the Auth → handler → Response
		// pipeline (OpSearch); fans ctx.Search out over GlobalSearchable models.
		if h.globalSearch != nil {
			r.Get(h.globalSearch.Path, h.GlobalSearch(*h.globalSearch))
		}

		// File upload/download/delete endpoints (only when storage is configured).
		// Each handler is wrapped in cfg.FileMiddleware so callers can apply
		// auth (e.g. auth.JWTAuth) to the standalone /files routes — the
		// per-model attachment routes already run through the full pipeline.
		if cfg.FilesConfig.MountEndpoints {
			mountFileEndpoints(r, cfg, l)
		}

		// One sub-router per registered model. Headless models register fully but
		// mount no REST routes, freeing their table path for a custom action.
		storageConfigured := cfg.FilesConfig.Storage != nil
		for _, meta := range reg.All() {
			if meta.Config.Headless {
				continue
			}
			mountModel(r, meta, h, storageConfigured)
		}

		// Custom action endpoints — mounted after model routes
		for _, action := range actions {
			sm := actionSyntheticModel(action.Method, action.Path)
			r.Method(action.Method, action.Path, h.Action(action, sm))
		}
	})

	mountStatic(r, cfg, l)

	return r
}

// collectRouterIssues reports the two router-level configuration problems that
// Config.Strict promotes to startup failures (10.1).
//
// Both stay warnings by default because each has a legitimate reading. An
// unauthenticated /files mount may be a deliberately public upload endpoint,
// unwise as that usually is. And a missing static directory currently degrades
// to 404s on /static/*, so failing the boot would let an absent frontend asset
// bundle take down a working API — an operational failure mode, not a typo.
//
// Under Strict, both become errors: in an environment where a boot failure costs
// a CI re-run rather than an outage, "probably wrong" is worth stopping for.
// staticPrefix resolves the URL prefix static files are served under,
// guaranteeing a leading slash so a bare prefix ("assets") does not panic chi.
// Shared by the mount and its validation so the two cannot disagree about what
// the configuration means.
func staticPrefix(cfg *Config) string {
	prefix := cfg.StaticPrefix
	if prefix == "" {
		prefix = "/static"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix
}

func collectRouterIssues(cfg *Config, issues *issueList) {
	if !cfg.Strict {
		return
	}
	if cfg.FilesConfig.MountEndpoints && len(cfg.FilesConfig.BeforeMiddlewares) == 0 {
		issues.addStrict("files",
			"the standalone /files endpoints are mounted with no auth middleware, so anyone "+
				"who can reach the API can upload, download, or delete files — set "+
				"FilesConfig.BeforeMiddlewares (e.g. auth.JWTAuth)")
	}
	if !cfg.StaticDisabled && cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err != nil {
			issues.addStrict("static",
				"Config.StaticDir %q does not exist, so nothing would be served under %s",
				cfg.StaticDir, staticPrefix(cfg))
		}
	}
}

// mountFileEndpoints registers the standalone /files upload/download/delete
// routes. They bypass the model pipeline, so auth has to come from
// FilesConfig.BeforeMiddlewares; an empty chain is warned about because it
// leaves /files open to anyone who can reach the API (SEC-4). Config.Strict
// makes that warning fatal — see collectRouterIssues.
func mountFileEndpoints(r chi.Router, cfg *Config, l *slog.Logger) {
	if len(cfg.FilesConfig.BeforeMiddlewares) == 0 {
		l.Warn("standalone /files endpoints mounted without auth middleware; "+
			"anyone who can reach the API can upload, download, or delete files",
			slog.String("hint", "set FilesConfig.BeforeMiddlewares (e.g. auth.JWTAuth)"))
	}
	fh := newFileHandlers(cfg.FilesConfig)
	r.Method(http.MethodPost, "/files", wrapFileMiddleware(cfg, fh.Upload))
	r.Method(http.MethodGet, "/files/*", wrapFileMiddleware(cfg, fh.Serve))
	r.Method(http.MethodDelete, "/files/*", wrapFileMiddleware(cfg, fh.Delete))
}

// mountStatic serves cfg.StaticDir under cfg.StaticPrefix at the router root
// (outside PathPrefix). Static serving is opt-in: it mounts only when StaticDir
// names a directory. StaticPrefix defaults to "/static" (set in ApplyDefaults).
// A named directory that does not exist is skipped with a warning; StaticDisabled
// turns serving off even when StaticDir is set.
//
// It used to fall back to "<cwd>/static" when StaticDir was empty, so a static/
// directory that merely happened to be in the working tree — a build cache, a
// checked-out asset bundle — was published at /static/ without anyone asking for
// it (DX-6). An empty StaticDir now serves nothing.
func mountStatic(r chi.Router, cfg *Config, l *slog.Logger) {
	if cfg.StaticDisabled || cfg.StaticDir == "" {
		return
	}

	prefix := staticPrefix(cfg)

	if _, err := os.Stat(cfg.StaticDir); err == nil {
		fileServer(r, prefix, http.Dir(cfg.StaticDir))
	} else {
		l.Warn("Static file path does not exist; skip mounting static",
			slog.String("path", cfg.StaticDir), slog.String("prefix", prefix))
	}
}

func fileServer(r chi.Router, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("FileServer does not permit any URL parameters.")
	}

	// Create a new file server handler
	fs := http.FileServer(root)

	// If the path is not a trailing slash, add one
	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", http.StatusMovedPermanently).ServeHTTP)
		path += "/"
	}

	// Mount the file server handler
	r.Handle(path+"*", http.StripPrefix(path, fs))
}

// mountModel registers five REST endpoints for one model under its table name.
//
//	GET    /{table}         → list   (pagination, filters, sorts, includes)
//	POST   /{table}         → create
//	GET    /{table}/{id}    → read   (includes supported via ?include=)
//	PATCH  /{table}/{id}    → update (partial update)
//	DELETE /{table}/{id}    → delete (hard or soft, per model config)
func mountModel(r chi.Router, meta *ModelMeta, h *handlers, storageConfigured bool) {
	base := fmt.Sprintf("/%s", strings.TrimPrefix(meta.TableName, TABLE_NAME_PREFIX))

	// Singleton models (ModelConfig.Singleton) expose a single row through GET
	// and PATCH on the bare path — no id, no POST/DELETE/list, no /{id} subtree.
	if meta.Config.Singleton {
		r.Route(base, func(r chi.Router) {
			r.Get("/", h.SingletonRead(meta))
			r.Patch("/", h.SingletonUpdate(meta))
			r.Head("/", h.Head(meta))
			r.Options("/", h.Options(meta))
		})
		return
	}

	r.Route(base, func(r chi.Router) {
		r.Get("/", h.List(meta))
		r.Post("/", h.Create(meta))
		r.Head("/", h.Head(meta))
		r.Options("/", h.Options(meta))

		// Auto-generated export endpoint (8.3). Mounted only when the model
		// opts in; runs through the standard pipeline so Auth, tenancy, and
		// soft-delete middleware apply.
		if meta.Config.ExportEnabled {
			r.Get("/export", h.Export(meta))
		}

		// Auto-generated aggregation endpoint (4.7). Mounted only when the model
		// opts in via ModelConfig.AggregateEnabled. Dispatches as OpList so the
		// list auth/tenancy middleware apply; ?aggregate= carries the query as
		// URL-encoded JSON.
		if meta.Config.AggregateEnabled {
			r.Get("/aggregate", h.Aggregate(meta))
		}

		// Presigned-upload mint route (R5). One POST per file field that opts in
		// with mfx:"upload:presigned". It carries no {id}: the record need not
		// exist, which is what lets a create-time file field use it at all.
		// Mounted only when storage is configured, for the same reason as the
		// attachment routes — a 404 is more honest than a 501 on every request.
		if storageConfigured {
			for _, ff := range meta.FileFields() {
				if ff.Tags.PresignedUpload {
					r.Post("/"+ff.Tags.JSONName+"/upload-url", h.PresignUpload(meta, ff))
				}
			}
		}

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Read(meta))
			r.Patch("/", h.Update(meta))
			r.Delete("/", h.Delete(meta))
			r.Head("/", h.Head(meta))
			r.Options("/", h.Options(meta))

			// Soft-delete restore route (5.19). Opt-in via
			// ModelConfig.RestoreEnabled, and only for models that soft-delete —
			// a hard delete leaves nothing to restore. Dispatches as OpUpdate so
			// existing update middleware governs it; see ServerContext.IsRestore.
			if meta.Config.RestoreEnabled && meta.SoftDelete.Enabled {
				r.Post("/restore", h.Restore(meta))
			}

			// Per-record version history (audit MS-4). The synthesized history
			// model is Headless, so this is the only way in — and it reuses the
			// parent's read pipeline, so the parent's auth and tenancy decide
			// who may see it. Scoping the history table directly is not possible:
			// it holds none of the parent's columns.
			if meta.Config.Versioned {
				r.Get("/history", h.History(meta))
			}

			// Per-model attachment route (3B.3a). One GET per file field on
			// the model, using the field's JSON name as the URL segment. Only
			// mounted when FileStorage is configured — otherwise the route
			// would return 501 on every request, so a 404 from chi is more
			// honest. Reuses the read pipeline so Auth and tenancy apply.
			// A FileKeys field mounts none: the route streams one object's bytes
			// and a list names no single one. Its keys reach the client through
			// the record (file_acl:signed rewrites each to a URL).
			if storageConfigured {
				for _, ff := range meta.FileFields() {
					if ff.IsFileList() {
						continue
					}
					r.Get("/"+ff.Tags.JSONName, h.Attachment(meta, ff))
				}
			}
		})
	})
}
