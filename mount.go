package maniflex

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers each Server instance on r at its configured PathPrefix.
// It lets multiple services share a single chi router so that shared
// middleware (logging, tracing, CORS) is applied once:
//
//	r := chi.NewRouter()
//	r.Use(myLoggingMiddleware, myTracingMiddleware)
//	maniflex.Mount(r,
//	    ordersService,    // PathPrefix: /api/orders
//	    inventoryService, // PathPrefix: /api/inventory
//	    ledgerService,    // PathPrefix: /api/ledger
//	)
//	http.ListenAndServe(":8080", r)
//
// Only PathPrefix is forwarded. [Config.StaticDir] is served at
// [Config.StaticPrefix], which sits outside PathPrefix so assets keep short
// URLs, so a mounted server answers 404 for files it serves standalone. Mount
// warns when that applies; serve the directory from the outer router, or set
// [Config.StaticDisabled] when something else already does.
func Mount(r chi.Router, services ...*Server) {
	for _, svc := range services {
		prefix := svc.cfg.PathPrefix
		inner := svc.Handler()

		// Handler registers the static tree at the router root, the one route it
		// puts outside PathPrefix — and PathPrefix is all that is forwarded below.
		// Assets therefore go dark with nothing to notice it by: a 404 at a path
		// that works when the same server runs standalone (audit HTTP-11).
		if svc.cfg.StaticDir != "" && !svc.cfg.StaticDisabled {
			svc.cfg.logger().Warn("Config.StaticDir is unreachable through maniflex.Mount: "+
				"static files are served outside PathPrefix, and Mount forwards PathPrefix only",
				slog.String("static_prefix", staticPrefix(&svc.cfg)),
				slog.String("path_prefix", prefix),
				slog.String("hint", "serve the directory from the outer router, or set "+
					"Config.StaticDisabled when something else already serves it"))
		}
		// chi.Mount strips PathPrefix from req.URL.Path before dispatching.
		// the Server handler registers its own routes under PathPrefix, so we restore
		// the stripped prefix and reset the chi RouteContext for clean re-routing.
		r.Mount(prefix, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// chi.Mount sets a stripped RouteContext for the inner handler, but
			// req.URL.Path is left intact. the Server handler has its own routes
			// registered under PathPrefix and expects to route from scratch, so
			// we reset the RouteContext to let it re-derive routing from the URL.
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, chi.NewRouteContext()))
			inner.ServeHTTP(w, req)
		}))
	}
}
