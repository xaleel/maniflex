package auth

import (
	"errors"
	"net/http"

	"github.com/xaleel/maniflex"
)

// Policy is an authorization function that determines whether the authenticated
// principal may proceed with the current operation on the given resource.
//
// resource carries the affected record's fields:
//   - OpCreate:        the proposed new record (ctx.ParsedBody)
//   - OpRead:          the fetched record (evaluated after the DB step)
//   - OpList:          each row in the result, evaluated one at a time
//   - OpUpdate/Delete: the current stored record, fetched before the write
//
// Return (false, nil) to produce a 403 FORBIDDEN response.
// Return a non-nil error to propagate a 500.
type Policy func(ctx *maniflex.ServerContext, resource map[string]any) (allow bool, err error)

// Enforce returns a DB-step middleware that evaluates p on every request.
//
// Behaviour by operation:
//
//   - OpCreate:        p is checked against ctx.ParsedBody before the insert.
//   - OpRead:          the DB step runs first; p is checked against the result.
//   - OpList:          the DB step runs first; p is applied to each row and
//     denied rows are removed from the result.
//   - OpUpdate/Delete: the current record is fetched and checked before the write.
//
// Register on the DB pipeline step, scoped with ForModel/ForOperation as needed:
//
//	server.Pipeline.DB.Register(
//	    auth.Enforce(myPolicy),
//	    maniflex.ForModel("Patient"),
//	)
//
// For list operations p is evaluated per row after the database query, so
// pagination totals reflect the pre-filter count. Use db.ForceFilter for
// efficient row-level scoping when the policy can be expressed as a WHERE clause.
func Enforce(p Policy) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		switch ctx.Operation {
		case maniflex.OpCreate:
			return enforceOnBody(ctx, p, next)
		case maniflex.OpUpdate, maniflex.OpDelete:
			return enforceBeforeWrite(ctx, p, next)
		case maniflex.OpRead:
			return enforceAfterRead(ctx, p, next)
		case maniflex.OpList:
			return enforceAfterList(ctx, p, next)
		default:
			return next()
		}
	}
}

// AllOf returns a Policy that allows only when every policy in ps allows.
// Evaluation stops at the first denial.
func AllOf(ps ...Policy) Policy {
	return func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
		for _, p := range ps {
			ok, err := p(ctx, resource)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
}

// AnyOf returns a Policy that allows when at least one policy in ps allows.
// Evaluation stops at the first approval.
func AnyOf(ps ...Policy) Policy {
	return func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
		for _, p := range ps {
			ok, err := p(ctx, resource)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
}

// Not returns a Policy that inverts p.
func Not(p Policy) Policy {
	return func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
		ok, err := p(ctx, resource)
		return !ok, err
	}
}

// ── internal helpers ──────────────────────────────────────────────────────────

// enforceOnBody checks p against ctx.ParsedBody before calling next.
// Used for OpCreate where no existing record exists yet.
func enforceOnBody(ctx *maniflex.ServerContext, p Policy, next func() error) error {
	allowed, err := p(ctx, ctx.ParsedBody.Map())
	if err != nil {
		return err
	}
	if !allowed {
		ctx.Abort(http.StatusForbidden, "FORBIDDEN", "access denied by policy")
		return nil
	}
	return next()
}

// enforceBeforeWrite fetches the current record, checks p, then calls next.
// Used for OpUpdate and OpDelete to prevent unauthorized writes.
func enforceBeforeWrite(ctx *maniflex.ServerContext, p Policy, next func() error) error {
	if ctx.ResourceID == "" {
		return next()
	}
	record, err := ctx.GetModel(ctx.Model.Name).Read(ctx.ResourceID)
	if errors.Is(err, maniflex.ErrNotFound) {
		return next() // let the DB step produce the 404
	}
	if err != nil {
		return err
	}
	allowed, err := p(ctx, record)
	if err != nil {
		return err
	}
	if !allowed {
		ctx.Abort(http.StatusForbidden, "FORBIDDEN", "access denied by policy")
		return nil
	}
	return next()
}

// enforceAfterRead calls next (running the DB step), then checks p against the
// fetched record. Used for OpRead.
func enforceAfterRead(ctx *maniflex.ServerContext, p Policy, next func() error) error {
	if err := next(); err != nil {
		return err
	}
	if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
		return nil // 404 or other error already set by the DB step
	}
	if ctx.DBResult == nil {
		return nil
	}
	// DBResult is the typed record (*T) on the read fast path, or a map for
	// encrypted/synthetic models; RecordToMap normalises both to a map view.
	record := maniflex.RecordToMap(ctx.Model, ctx.DBResult)
	if record == nil {
		return nil
	}
	allowed, err := p(ctx, record)
	if err != nil {
		return err
	}
	if !allowed {
		ctx.Abort(http.StatusForbidden, "FORBIDDEN", "access denied by policy")
	}
	return nil
}

// enforceAfterList calls next (which runs the DB step and the Response step),
// then filters ctx.Response.Data by calling p on each JSON-keyed row map.
// Rows denied by p are removed from the response. Pagination totals are not
// adjusted — they reflect the pre-filter DB count. This is an inherent
// limitation of post-fetch policy evaluation; use db.ForceFilter for accurate
// totals when the policy can be expressed as a WHERE clause.
func enforceAfterList(ctx *maniflex.ServerContext, p Policy, next func() error) error {
	if err := next(); err != nil {
		return err
	}
	if ctx.Response == nil || ctx.Response.StatusCode >= 400 {
		return nil
	}
	items, ok := ctx.Response.Data.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	kept := items[:0]
	for _, item := range items {
		row, _ := item.(map[string]any)
		allowed, err := p(ctx, row)
		if err != nil {
			return err
		}
		if allowed {
			kept = append(kept, item)
		}
	}
	ctx.Response.Data = kept
	return nil
}
