package maniflex

// scoped_tx.go — an ActionScope-aware Tx (R1 follow-up).
//
// ctx.BeginTx used to refuse outright while an ActionScope was in force, on the
// grounds that a Tx speaks to the adapter directly and so cannot be scoped. That
// was true of the handle but not of the interface: Tx mirrors DBAdapter —
// FindByID and FindMany take a *QueryParams, Update and Delete are keyed by id —
// which is exactly the shape the accessor already scopes. So it is scoped here
// instead, and three things follow.
//
// ctx.Tx is a public field. Refusing meant a scoped action wanting a transaction
// had to open one through ctx.Unscoped().BeginTx, which parked a raw, unscoped Tx
// on that public field for the rest of the request: anything downstream reading
// ctx.Tx got an unscoped handle, and ctx.Tx.Update(…) was an unscoped write
// inside a scoped request. That is the partial guarantee the fail-closed rule
// exists to avoid, so refusing was not actually the conservative choice.
//
// maniflex.WithTransaction and maniflex.Batch both call ctx.BeginTx, and both are
// registered as middleware — including in an ActionConfig.Middleware list, which
// is a natural thing to do. Refusing dead-ended them with no workaround: a
// middleware cannot be routed through Unscoped().
//
// And the ergonomics were self-defeating. ctx.Unscoped() earns its keep by being
// rare enough that a grep for it finds every deliberate bypass. Requiring it for
// routine transactional work would have made it background noise.
//
// ctx.Unscoped().BeginTx still returns an unscoped Tx — that is the deliberate
// bypass, unchanged.

import (
	"context"
	"database/sql"
)

// scopedTx wraps a Tx so every operation through it honours an ActionScope. It
// is applied by ServerContext.BeginTx and sits outermost, over tracedTx when
// tracing is on, so its overrides win and the trace still sees the real calls.
type scopedTx struct {
	Tx
	ctx   *ServerContext
	scope *ActionScope
}

// filters is the scope's filter set, nil when there is nothing to enforce.
func (t *scopedTx) filters() []*FilterExpr {
	if t.scope == nil {
		return nil
	}
	return t.scope.Filters
}

// inScope reports whether id names a record the scope admits, reading through
// the wrapped Tx so the check shares the transaction with the write it guards —
// the row cannot leave the scope in between. A miss returns ErrNotFound from the
// adapter, which is what the caller sees: the same answer the scoped read gives
// for that id, so a write discloses nothing a read would not.
func (t *scopedTx) inScope(ctx context.Context, model *ModelMeta, id string) error {
	sf := t.filters()
	if len(sf) == 0 {
		return nil
	}
	_, err := t.Tx.FindByID(ctx, model, id, &QueryParams{Page: 1, Limit: 1, Filters: sf})
	return err
}

func (t *scopedTx) FindByID(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (any, error) {
	return t.Tx.FindByID(ctx, model, id, withScopeFilters(q, t.filters()))
}

func (t *scopedTx) FindMany(ctx context.Context, model *ModelMeta, q *QueryParams) ([]any, int64, error) {
	return t.Tx.FindMany(ctx, model, withScopeFilters(q, t.filters()))
}

// FindByIDForUpdate takes no *QueryParams — it is the pessimistic-lock read —
// so the scope is applied by looking the record up through it first, inside the
// same transaction.
func (t *scopedTx) FindByIDForUpdate(ctx context.Context, model *ModelMeta, id string) (any, error) {
	if err := t.inScope(ctx, model, id); err != nil {
		return nil, err
	}
	return t.Tx.FindByIDForUpdate(ctx, model, id)
}

func (t *scopedTx) Create(ctx context.Context, model *ModelMeta, record any) (any, error) {
	if err := stampScope(t.ctx, model, record); err != nil {
		return nil, err
	}
	return t.Tx.Create(ctx, model, record)
}

func (t *scopedTx) Update(ctx context.Context, model *ModelMeta, id string, record any,
	present map[string]struct{},
) (any, error) {
	if err := t.inScope(ctx, model, id); err != nil {
		return nil, err
	}
	return t.Tx.Update(ctx, model, id, record, present)
}

func (t *scopedTx) Delete(ctx context.Context, model *ModelMeta, id string) error {
	if err := t.inScope(ctx, model, id); err != nil {
		return err
	}
	return t.Tx.Delete(ctx, model, id)
}

// RawQueryContext / RawExecContext / ExecContext forward the contracts callers
// reach by type-asserting on the Tx. Embedding the Tx interface promotes only
// the methods Tx itself declares, so without these a scoped transaction would
// not satisfy rawableT — and ctx.Unscoped().RawQuery inside one would fail with
// ErrRawNotSupportedInTx, or worse fall out of the transaction onto the bare
// adapter. That is BUG-12, which tracedTx already had to fix for exactly the
// same reason; a second wrapper is a second chance to reintroduce it.
//
// The SQL run through these is not scoped, and is not meant to be: ctx.RawQuery
// refuses under a scope, so the only way to reach them is ctx.Unscoped(), which
// is the caller saying so.
func (t *scopedTx) RawQueryContext(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rt, ok := t.Tx.(rawableT)
	if !ok {
		return nil, ErrRawNotSupportedInTx
	}
	return rt.RawQueryContext(ctx, query, args...)
}

func (t *scopedTx) RawExecContext(ctx context.Context, query string, args ...any) (int64, error) {
	rt, ok := t.Tx.(rawableT)
	if !ok {
		return 0, ErrRawNotSupportedInTx
	}
	return rt.RawExecContext(ctx, query, args...)
}

func (t *scopedTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if ex, ok := t.Tx.(sqlExecContexter); ok {
		return ex.ExecContext(ctx, query, args...)
	}
	return nil, ErrRawNotSupportedInTx
}

// withScopeFilters returns q with sf AND-ed in, without mutating q — a caller
// reusing one *QueryParams across calls must not accumulate a copy of the scope
// on each. Returns q untouched when there is nothing to add.
func withScopeFilters(q *QueryParams, sf []*FilterExpr) *QueryParams {
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
