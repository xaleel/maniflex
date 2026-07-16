package db

// action_scope.go — the Action counterparts of ForceFilter and Tenancy (R1).
//
// ForceFilter and Tenancy register on the DB step, which an Action does not run:
// its pipeline is Auth → middleware → handler → Response. Their only output is a
// filter on ctx.Query, and nothing in that chain reads it, so a
// `Pipeline.DB.Register(db.Tenancy(...))` is simply inert for every Action —
// silently, because the ineffective-registration warning only fires for a
// middleware whose operation filter names *only* skipped steps, and the
// idiomatic registration names none at all.
//
// These set a maniflex.ActionScope instead, which the DB paths an action handler
// can reach consult directly. Register them in the action's own Middleware list,
// where they run after Auth and so can read ctx.Auth:
//
//	server.Action(maniflex.ActionConfig{
//	    Method: "POST", Path: "/orders/{id}/refund",
//	    Middleware: []maniflex.MiddlewareFunc{
//	        auth.JWTAuth(secret),
//	        db.TenancyAction("org_id", orgOf),
//	    },
//	    Handler: refund,
//	})
//
// Inside that handler ctx.GetModel, the typed generics, ctx.Aggregate and
// ctx.LockForUpdate are scoped to the tenant, and ctx.RawQuery, ctx.RawExec,
// ctx.BeginTx and ctx.Search refuse rather than run unscoped — see
// maniflex.ActionScope.

import (
	"net/http"

	"github.com/xaleel/maniflex"
)

// ForceFilterAction pins an Action's DB access to `field = fn(ctx)`.
//
// It is the Action counterpart of ForceFilter, and unlike it must be registered
// in the action's Middleware list rather than on the DB step. When fn returns
// nil the request is left unscoped — the same "no value, no filter" rule
// ForceFilter follows, which is what lets a scope apply to some callers and not
// others (an admin, a service token).
func ForceFilterAction(field string, fn ForceFilterFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		val := fn(ctx)
		if val == nil {
			return next()
		}
		ctx.SetActionScope(&maniflex.ActionScope{
			Name: "a forced filter on " + field,
			Filters: []*maniflex.FilterExpr{{
				Field:    field,
				Operator: maniflex.OpEq,
				Value:    val,
				Forced:   true,
			}},
		})
		return next()
	}
}

// TenancyAction pins an Action's DB access to the caller's tenant.
//
// It is the Action counterpart of Tenancy. As there, a caller whose tenant
// cannot be determined is refused with 403 rather than left unscoped — an
// unidentifiable tenant is the case where running unscoped would be worst.
//
//	server.Action(maniflex.ActionConfig{
//	    Method: "POST", Path: "/orders/{id}/refund",
//	    Middleware: []maniflex.MiddlewareFunc{
//	        auth.JWTAuth(secret),
//	        db.TenancyAction("org_id", func(ctx *maniflex.ServerContext) string {
//	            return ctx.Auth.Claims["org_id"].(string)
//	        }),
//	    },
//	    Handler: refund,
//	})
func TenancyAction(tenantField string, fn TenantFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		tenantID := fn(ctx)
		if tenantID == "" {
			ctx.Abort(http.StatusForbidden, "FORBIDDEN", "tenant identity could not be determined")
			return nil
		}
		ctx.SetActionScope(&maniflex.ActionScope{
			Name: "tenancy on " + tenantField,
			Filters: []*maniflex.FilterExpr{{
				Field:    tenantField,
				Operator: maniflex.OpEq,
				Value:    tenantID,
				Forced:   true,
			}},
		})
		return next()
	}
}
