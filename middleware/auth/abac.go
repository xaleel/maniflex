package auth

import (
	"errors"
	"net/http"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/internal/accessdecision"
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
// On update that is the row as it stands, not as the request would leave it: a
// policy approving "your own record" also approves a PATCH that gives the record
// to someone else. Read the values being written with ctx.Field when the rule is
// about the result.
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
//   - History/attachment reads: the parent record is fetched and checked before
//     the DB step, since what that step produces is history rows or file bytes
//     rather than the record the policy is about.
//   - OpSearch/OpExport: refused with 403. Both return rows the policy cannot be
//     applied to; scope them with db.ForceFilter or db.Tenancy instead.
//
// Every other operation — custom actions, presigned uploads, HEAD, OPTIONS — is
// passed through untouched. Enforce is a policy on a model's records, and those
// carry no record for it to decide about.
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
	mw := func(ctx *maniflex.ServerContext, next func() error) error {
		switch ctx.Operation {
		case maniflex.OpCreate:
			return enforceOnBody(ctx, p, next)
		case maniflex.OpUpdate, maniflex.OpDelete:
			if ctx.IsRestore() {
				// A restore dispatches as OpUpdate against a soft-deleted row, which
				// every read path hides — so the fetch below returned ErrNotFound and
				// was read as "the DB step will 404 this", and the DB step then
				// restored the row with the policy never evaluated. Any authenticated
				// caller could un-delete any record by id, and the response returned
				// the whole row with it (audit AUTH-10).
				//
				// Refused rather than guessed at. A Policy is arbitrary Go over a
				// record map, so it cannot be pushed into the restore statement the
				// way an equality can — which is how db.ForceFilter, db.Tenancy and
				// RequireOwner scope a restore correctly.
				ctx.Abort(http.StatusForbidden, "FORBIDDEN",
					"auth.Enforce cannot evaluate a policy on a restore: its target is "+
						"soft-deleted and so unreadable here. Scope restores with "+
						"db.ForceFilter or db.Tenancy, which filter in SQL.")
				return nil
			}
			return enforceOnStoredRecord(ctx, p, next)
		case maniflex.OpRead:
			return enforceAfterRead(ctx, p, next)
		case maniflex.OpList:
			return enforceAfterList(ctx, p, next)
		case maniflex.OpReadHistory, maniflex.OpReadAttachment:
			// Checked against the parent record before the DB step, not after it:
			// what the DB step produces here is history rows, or a file streamed
			// straight to the response writer, and neither is the record the policy
			// is written about. These used to reach default and pass, so a policy
			// that refused a direct read still served that record's diffs and
			// attachments to anyone (audit AUTH-2).
			return enforceOnStoredRecord(ctx, p, next)
		case maniflex.OpSearch, maniflex.OpExport:
			// Refused rather than passed. Both return rows the policy never sees:
			// export streams bytes with ctx.Response left nil, so there is nothing
			// for enforceAfterList to filter, and search builds its result outside
			// the list path. Passing them meant ForOperation(OpList) — which covers
			// OpExport by alias — filtered the list and streamed the whole table at
			// /export.
			ctx.Abort(http.StatusForbidden, "FORBIDDEN",
				"auth.Enforce cannot evaluate a policy on "+string(ctx.Operation)+
					": it returns rows the policy never sees. Scope this route with "+
					"db.ForceFilter or db.Tenancy instead, registered where the "+
					"operation runs — the DB step for an export, Pipeline.Auth for a "+
					"search.")
			return nil
		default:
			return next()
		}
	}
	// A policy is not a filter, so this cannot scope GET /search at all — it can
	// only refuse it, and only from a step that endpoint runs. Marked so the
	// startup check can say which model is left unscoped and where to move the
	// registration (audit AUTH-20).
	accessdecision.MarkRowScope(mw, "auth.Enforce")
	return mw
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

// enforceOnStoredRecord fetches the record targeted by ctx.ResourceID, checks p
// against it, then calls next. Used wherever the policy has to be decided before
// the DB step runs: the writes (OpUpdate, OpDelete), and the derived reads whose
// DB step produces something other than the record itself (OpReadHistory,
// OpReadAttachment).
func enforceOnStoredRecord(ctx *maniflex.ServerContext, p Policy, next func() error) error {
	if ctx.ResourceID == "" {
		return next()
	}
	record, err := ctx.GetModel(ctx.Model.Name).Read(ctx.ResourceID)
	if errors.Is(err, maniflex.ErrNotFound) {
		switch ctx.Operation {
		case maniflex.OpReadHistory, maniflex.OpReadAttachment:
			// Falling through to next() here assumes the DB step will answer 404
			// for the same row this fetch could not find. For the derived reads
			// that assumption does not hold: the history query deliberately omits
			// the soft-delete condition, because "a soft-deleted record's history
			// must stay readable by whoever could read the record" — so a record
			// the policy guards while it is live lost that guard the moment it was
			// deleted, and any authenticated caller could read its full field-level
			// diffs, values included (audit AUTH-10).
			//
			// Refused instead. The parent is unreadable to this middleware, so the
			// policy cannot be evaluated, so nothing derived from it is served. The
			// attachment path does 404 on a soft-deleted row today, via FindByID;
			// it is refused here too rather than left resting on that.
			ctx.Abort(http.StatusNotFound, "NOT_FOUND", "record not found")
			return nil
		}
		return next() // a write: the DB step produces the 404
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
