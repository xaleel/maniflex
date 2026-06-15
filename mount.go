package maniflex

import (
	"context"
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
func Mount(r chi.Router, services ...*Server) {
	for _, svc := range services {
		prefix := svc.cfg.PathPrefix
		inner := svc.Handler()
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
