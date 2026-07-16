package maniflex

// action_scope.go — row-level scoping for custom Actions (R1).
//
// The generated CRUD routes get their scoping from the DB step: db.Tenancy and
// db.ForceFilter append a Forced filter to ctx.Query, the read path puts it in
// the query and the write path pre-flights it (enforceWriteScope). An Action
// runs a trimmed pipeline — Auth → middleware → handler → Response — with the
// DB step declared skipped (stepsSkippedByOp), so none of that applies, and a
// db.Tenancy registered on the DB step is simply inert for it. Nothing warns,
// because warnIneffectiveMiddleware only fires for a middleware whose operation
// filter names *only* skipped steps, and the idiomatic registration names none.
//
// So an action carries its scope on the context instead, and every DB path an
// action can reach consults it. The paths divide in two:
//
//   - Those that can carry a filter — ctx.GetModel's accessor, the typed
//     List/Read/Create/Update/Delete generics, ctx.Aggregate, ctx.LockForUpdate —
//     apply the scope, and a write to a record outside it is ErrNotFound, the
//     same answer a scoped read gives.
//   - Those that cannot — ctx.RawQuery, ctx.RawExec, ctx.BeginTx, ctx.Search —
//     refuse while a scope is active, rather than running unscoped.
//
// The refusal is the point. Scoping only what is convenient and leaving raw SQL
// to leak in silence would be a guarantee in the documentation and not in the
// code — worse than no guarantee, because it is trusted. Refusing means an
// action either honours the scope or fails loudly at the first request that
// exercises it, and ctx.Unscoped() makes the bypass an explicit, greppable
// decision instead of an oversight.
//
// This is deliberately not wired into the CRUD path: there, the DB step is the
// enforcement point and ctx.RawQuery from an After-DB middleware is a normal
// thing to do.

import (
	"context"
	"fmt"
	"strings"
)

// ActionScope is the row-level scope in force for a custom Action. It is set by
// db.TenancyAction / db.ForceFilterAction (or by hand, via
// ServerContext.SetActionScope) and read by every DB path reachable from an
// action handler.
type ActionScope struct {
	// Filters constrain every read, and every write is refused unless the target
	// record matches them. They are AND-ed with whatever the caller asked for.
	Filters []*FilterExpr

	// Name identifies the scope in the error a refused path returns, so the
	// message says what is being enforced rather than only that something is.
	// db.TenancyAction sets "tenancy"; empty reads as "an access scope".
	Name string
}

// scopeName renders the scope for an error message.
func (s *ActionScope) scopeName() string {
	if s == nil || s.Name == "" {
		return "an access scope"
	}
	return s.Name
}

// injectable returns the columns a create must be stamped with for the record to
// satisfy the scope: an equality filter on a plain column says both "only rows
// where field = value" and "a row you create has field = value". Anything else —
// an operator that is not equality, a nested or locale filter — cannot be
// turned into a value, and createGuard rejects a create it cannot satisfy rather
// than writing a row the scope would then hide from its author.
func (s *ActionScope) injectable() (map[string]any, error) {
	if s == nil {
		return nil, nil
	}
	out := make(map[string]any, len(s.Filters))
	for _, f := range s.Filters {
		if f == nil {
			continue
		}
		if f.Operator != OpEq || f.IsNested || f.IsLocale || f.Group > 0 {
			return nil, fmt.Errorf(
				"maniflex: %s cannot be applied to a create: the scope filter on %q is not a "+
					"plain equality, so there is no value to store. Set the column on the record "+
					"yourself, or scope the create with ctx.Unscoped()",
				s.scopeName(), f.Field)
		}
		out[f.Field] = f.Value
	}
	return out, nil
}

// SetActionScope pins a row-level scope to this request. Every DB path an action
// handler can reach then either applies it or refuses to run — see ActionScope.
//
// Middleware registered in an ActionConfig.Middleware list calls this;
// db.TenancyAction and db.ForceFilterAction are the shipped wrappers. Calling it
// twice replaces the scope rather than merging, so two scoping middlewares on
// one action is a mistake this makes visible rather than silently AND-ing.
func (c *ServerContext) SetActionScope(s *ActionScope) { c.actionScope = s }

// ActionScope returns the scope in force, or nil when the request is unscoped.
func (c *ServerContext) ActionScope() *ActionScope { return c.actionScope }

// scopeFilters returns the scope's filters, or nil when there is no scope. The
// returned slice must not be mutated by callers — it is shared with the scope.
func (c *ServerContext) scopeFilters() []*FilterExpr {
	if c.actionScope == nil {
		return nil
	}
	return c.actionScope.Filters
}

// scopedQuery returns q with the scope's filters AND-ed in, without mutating the
// caller's q — a caller reusing one *QueryParams across calls must not
// accumulate a copy of the scope per call. Returns q untouched when unscoped.
func (c *ServerContext) scopedQuery(q *QueryParams) *QueryParams {
	sf := c.scopeFilters()
	if len(sf) == 0 {
		return q
	}
	if q == nil {
		q = &QueryParams{Page: 1, Limit: defaultLimit}
	}
	clone := *q
	clone.Filters = make([]*FilterExpr, 0, len(q.Filters)+len(sf))
	clone.Filters = append(clone.Filters, q.Filters...)
	clone.Filters = append(clone.Filters, sf...)
	return &clone
}

// guardRaw is the refusal returned by a path that cannot honour the scope. It
// names the path, the scope, and the way out, because the caller reading it is
// deciding between "scope this differently" and "this genuinely must bypass".
func (c *ServerContext) guardRaw(path string) error {
	if c.actionScope == nil {
		return nil
	}
	return fmt.Errorf(
		"maniflex: %s is in force on this request and %s cannot enforce it — the scope is "+
			"applied to ctx.GetModel, the typed List/Read/Create/Update/Delete generics, "+
			"ctx.Aggregate and ctx.LockForUpdate, but not to %s. Rewrite the access through one "+
			"of those, or call ctx.Unscoped().%s to bypass the scope deliberately",
		c.actionScope.scopeName(), path, path, strings.TrimSuffix(path, "()")+"(…)")
}

// UnscopedContext is the deliberate bypass of an ActionScope. Obtain one from
// ServerContext.Unscoped. It exposes the paths that refuse while a scope is in
// force, and running them through it is a statement that the caller has
// considered the scope and is stepping outside it on purpose — an audit query
// across tenants, a migration, a health probe.
//
// It is a distinct type rather than a flag so the bypass appears at the call
// site, in the diff, and in a grep for "Unscoped".
type UnscopedContext struct{ c *ServerContext }

// Unscoped returns a handle that bypasses any ActionScope in force. On an
// unscoped request it is simply the same calls, so code need not branch on
// whether a scope happens to be set.
func (c *ServerContext) Unscoped() *UnscopedContext { return &UnscopedContext{c: c} }

// RawQuery runs ctx.RawQuery without the scope check. The rows are whatever the
// SQL selects, across every tenant.
func (u *UnscopedContext) RawQuery(query string, args ...any) ([]Row, error) {
	return u.c.rawQuery(query, args...)
}

// RawExec runs ctx.RawExec without the scope check.
func (u *UnscopedContext) RawExec(query string, args ...any) (int64, error) {
	return u.c.rawExec(query, args...)
}

// BeginTx opens a transaction without the scope check. Nothing done through the
// returned Tx is scoped — it speaks to the adapter directly.
func (u *UnscopedContext) BeginTx(ctx context.Context, opts *TxOptions) (Tx, error) {
	return u.c.beginTx(ctx, opts)
}

// GetModel returns an accessor with no scope applied.
func (u *UnscopedContext) GetModel(modelName string) *ModelAccessor {
	return u.c.getModel(modelName, nil)
}

// Search runs ctx.Search across every model's rows, unscoped.
func (u *UnscopedContext) Search(opts SearchOptions) ([]SearchResult, error) {
	return u.c.search(opts)
}
