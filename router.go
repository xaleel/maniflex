package maniflex

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	r.Use(chiMiddleware.RealIP)

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
			fh := newFileHandlers(cfg.FilesConfig)
			r.Method(http.MethodPost, "/files", wrapFileMiddleware(cfg, fh.Upload))
			r.Method(http.MethodGet, "/files/*", wrapFileMiddleware(cfg, fh.Serve))
			r.Method(http.MethodDelete, "/files/*", wrapFileMiddleware(cfg, fh.Delete))
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

// mountStatic serves cfg.StaticDir under cfg.StaticPrefix at the router root
// (outside PathPrefix). StaticDir defaults to "<cwd>/static" and StaticPrefix to
// "/static" (set in ApplyDefaults), preserving the historical mapping. A missing
// directory is skipped with a warning; StaticDisabled turns it off entirely.
func mountStatic(r chi.Router, cfg *Config, l *slog.Logger) {
	if cfg.StaticDisabled {
		return
	}

	staticPath := cfg.StaticDir
	if staticPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return
		}
		staticPath = filepath.Join(cwd, "static")
	}

	// Guarantee a leading slash so a bare prefix ("assets") doesn't panic chi.
	prefix := cfg.StaticPrefix
	if prefix == "" {
		prefix = "/static"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	if _, err := os.Stat(staticPath); err == nil {
		fileServer(r, prefix, http.Dir(staticPath))
	} else {
		l.Warn("Static file path does not exist; skip mounting static",
			slog.String("path", staticPath), slog.String("prefix", prefix))
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
		// list auth/tenancy middleware apply; the JSON body carries the query.
		if meta.Config.AggregateEnabled {
			r.Get("/aggregate", h.Aggregate(meta))
		}

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Read(meta))
			r.Patch("/", h.Update(meta))
			r.Delete("/", h.Delete(meta))
			r.Head("/", h.Head(meta))
			r.Options("/", h.Options(meta))

			// Per-model attachment route (3B.3a). One GET per file field on
			// the model, using the field's JSON name as the URL segment. Only
			// mounted when FileStorage is configured — otherwise the route
			// would return 501 on every request, so a 404 from chi is more
			// honest. Reuses the read pipeline so Auth and tenancy apply.
			if storageConfigured {
				for _, ff := range meta.FileFields() {
					r.Get("/"+ff.Tags.JSONName, h.Attachment(meta, ff))
				}
			}
		})
	})
}
