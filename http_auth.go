package maniflex

import (
	"fmt"
	"net/http"
)

// AdaptAuth adapts request-level pipeline authentication middleware for use as
// HTTP middleware. It is intended for router-level endpoints such as generated
// documentation, which have no model or CRUD operation but still need the same
// JWT, API-key, role, or scope checks as model routes.
//
// Middleware runs in the supplied order and shares one ServerContext, so an
// authenticator can populate Auth for a following authorizer:
//
//	cfg.Documentation.Middleware = []maniflex.HTTPMiddleware{
//	    maniflex.AdaptAuth(
//	        auth.JWTAuth(secret),
//	        auth.RequireRole("internal"),
//	    ),
//	}
//
// Do not adapt middleware that requires Model, DB, or CRUD operation state.
func AdaptAuth(middleware ...MiddlewareFunc) HTTPMiddleware {
	for i, mw := range middleware {
		if mw == nil {
			panic(fmt.Sprintf("maniflex: AdaptAuth middleware must not be nil at index %d", i))
		}
	}

	return func(next http.Handler) http.Handler {
		if next == nil {
			panic("maniflex: AdaptAuth next handler must not be nil")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := &ServerContext{
				Request: r,
				Writer:  w,
				Ctx:     r.Context(),
			}

			var run func(int) error
			run = func(i int) error {
				if i == len(middleware) {
					next.ServeHTTP(w, r)
					return nil
				}
				return middleware[i](ctx, func() error { return run(i + 1) })
			}

			if err := run(0); err != nil {
				ctx.Abort(http.StatusInternalServerError, "INTERNAL", err.Error())
			}
			if ctx.Response != nil {
				ctx.Response.Write(w)
			}
		})
	}
}
