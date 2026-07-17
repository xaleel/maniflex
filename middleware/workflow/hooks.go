package workflow

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/xaleel/maniflex"
)

// Hook is a side effect to run when a transition is taken. It runs inside the
// write's transaction, after the transition has been re-validated against the
// locked row and after the write itself has landed — so it sees the new state
// and anything it writes commits with the transition or not at all.
//
// Returning an error rolls the whole request back, transition included. A hook
// that wants a specific status can call ctx.Abort and return nil; an error with
// no response set becomes 500 WORKFLOW_HOOK_ERROR.
type Hook func(ctx *maniflex.ServerContext, from, to string) error

type hook struct {
	from string // "" or "*" means "any"
	to   string // "" or "*" means "any"
	fn   Hook
}

// OnTransition runs fn when the record moves from -> to. Either side may be "*"
// to match any state, and every matching hook fires in declaration order —
// unlike Allow, which is first-match-wins. That difference is deliberate:
// OnTransition("pending", "confirmed", applyCredit) and
// OnTransition("*", "delivered", emitReview) describe independent side effects,
// and a rule ordering that silenced one of them would be a bug, not a policy.
//
// Declaring a hook makes Middleware() panic: hooks require the DB step, where
// the write's transaction exists. Register Hooks() instead — it enforces the
// same rules and guards Middleware() does, and more strictly.
//
//	sm := workflow.New("status",
//	    workflow.Allow("pending", "confirmed", workflow.RequireRole("store_owner")),
//	    workflow.OnTransition("pending", "confirmed", applyStoreCredit),
//	    workflow.OnTransition("*", "delivered", emitReviewRequested),
//	)
//	server.Pipeline.DB.Register(sm.Hooks(), maniflex.ForModel("Order"))
func OnTransition(from, to string, fn Hook) Option {
	return func(m *Machine) {
		m.hooks = append(m.hooks, hook{from: from, to: to, fn: fn})
	}
}

// hooksFor returns every hook matching from -> to, in declaration order.
func (m *Machine) hooksFor(from, to string) []Hook {
	var out []Hook
	for _, h := range m.hooks {
		if (h.from == "*" || h.from == from) && (h.to == "*" || h.to == to) {
			out = append(out, h.fn)
		}
	}
	return out
}

// Hooks returns the DB-step MiddlewareFunc that enforces this Machine and fires
// its OnTransition hooks. Register it on the DB step, at the default Before
// position, with maniflex.WithTransaction on the Service step:
//
//	server.Pipeline.Service.Register(maniflex.WithTransaction(nil),
//	    maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpUpdate))
//	server.Pipeline.DB.Register(sm.Hooks(), maniflex.ForModel("Order"))
//
// Hooks supersedes Middleware rather than supplementing it. Middleware reads
// `from` on the Validate step, which runs before WithTransaction opens the
// transaction — so that read takes no lock and two concurrent PATCHes can both
// observe from="pending", both pass, and both fire the hook. Hooks re-reads
// `from` through ctx.LockForUpdate inside the transaction, which is what
// actually serialises them: the second waits, sees "confirmed", and is either a
// no-op (same-state) or re-checked against the rules as the transition it has
// really become. A plain in-transaction re-read would not be enough — under
// Postgres READ COMMITTED both readers still see "pending".
//
// This is why Hooks re-runs the rule match and its guards rather than trusting
// the Validate step's verdict: once `from` is re-read it may name a different
// rule, with different guards, than the one that passed on the stale value.
//
// Only OpUpdate is a transition. A Create seeds an initial state — AllowInitial
// governs it — so hooks do not fire on it; register a Service-step middleware
// if you need a side effect there.
func (m *Machine) Hooks() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil || ctx.Model == nil || ctx.Operation != maniflex.OpUpdate {
			return next()
		}
		to, present := stringField(ctx.ParsedBody.Map(), m.field)
		if !present || ctx.ResourceID == "" {
			// A PATCH that doesn't touch the status field transitions nothing.
			return next()
		}

		// Follow the lock_scope precedent (steps.go): fail loudly rather than
		// take no lock and silently reintroduce the race Hooks exists to close.
		if ctx.Tx == nil {
			ctx.Abort(http.StatusInternalServerError, "WORKFLOW_NO_TX",
				"workflow hooks require an active transaction; register maniflex.WithTransaction(nil) on the Service step")
			return nil
		}

		from, ok, err := m.lockedFrom(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return next() // record gone — let the DB step produce the standard 404
		}

		if err := next(); err != nil {
			return err
		}
		// The write was rejected downstream (a forced filter's 404, a constraint
		// violation). Nothing transitioned, so nothing is owed a hook — and the
		// rejection below would otherwise mask that response with our own.
		if ctx.Response != nil && ctx.Response.StatusCode >= http.StatusBadRequest {
			return nil
		}
		// A no-op write is not an attempted transition. This is also how a
		// concurrent duplicate PATCH lands once the lock lets it through.
		if from == to {
			return nil
		}
		return m.applyTransition(ctx, from, to)
	}
}

// lockedFrom re-reads the status field through a row lock inside ctx.Tx.
// Reports ok=false when the record does not exist.
func (m *Machine) lockedFrom(ctx *maniflex.ServerContext) (from string, ok bool, err error) {
	current, lerr := ctx.LockForUpdate(ctx.Model.Name, ctx.ResourceID)
	if lerr != nil {
		if errors.Is(lerr, maniflex.ErrNotFound) {
			return "", false, nil
		}
		ctx.Abort(http.StatusInternalServerError, "WORKFLOW_LOAD_ERROR", lerr.Error())
		return "", false, nil
	}
	from, _ = stringField(current, m.field)
	return from, true, nil
}

// applyTransition re-checks the transition against the locked `from` and fires
// every matching hook. Setting a 4xx or returning an error rolls the write back
// through WithTransaction, so a rejection here un-does the update next() made.
func (m *Machine) applyTransition(ctx *maniflex.ServerContext, from, to string) error {
	matched, guards := m.match(from, to)
	if !matched {
		rejectTransition(ctx, m.field, from, to)
		return nil
	}
	for _, g := range guards {
		if err := g.Check(ctx, from, to); err != nil {
			rejectGuard(ctx, m.field, from, to, err.Error())
			return nil
		}
	}
	for _, fn := range m.hooksFor(from, to) {
		if err := fn(ctx, from, to); err != nil {
			// A hook that set its own response keeps it; otherwise the error is
			// ours to report. Either way the transaction rolls back.
			if ctx.Response == nil || ctx.Response.StatusCode < http.StatusBadRequest {
				ctx.Abort(http.StatusInternalServerError, "WORKFLOW_HOOK_ERROR",
					fmt.Sprintf("transition %q → %q hook failed: %v", from, to, err))
			}
			return nil
		}
		// A hook may reject by aborting and returning nil.
		if ctx.Response != nil && ctx.Response.StatusCode >= http.StatusBadRequest {
			return nil
		}
	}
	return nil
}
