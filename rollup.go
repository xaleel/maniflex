package maniflex

import (
	"context"
	"fmt"
)

// Rollup declares a denormalised aggregate column on a parent model that the
// framework keeps in step with its children. It is the maintained form of the
// column an app would otherwise recompute by hand — `Order.PaidAmount` as
// `SUM(OrderPayment.amount)`, `StoreSite.ReviewsCount` as `COUNT(Review)` — and
// which drifts the moment one write path forgets to update it.
//
//	srv.MustRegisterRollup(maniflex.Rollup{
//	    Parent: "Order", ParentField: "paid_amount", Op: maniflex.AggSum,
//	    Child:  "OrderPayment", ChildField: "amount", On: "order_id",
//	})
//
// On every create, update or delete of a Child, the parent named by the child's
// `On` foreign key is recomputed from scratch — `Op(ChildField)` over that
// parent's live children — and written to `ParentField`, inside the request's
// transaction. Recomputing rather than applying a delta is what makes it
// correct by construction: a delete, a re-parenting update (the FK moved, so
// both the old and new parent are recomputed), and a soft-deleted child are all
// handled without a special case, and the total can never drift from the rows it
// summarises. Soft-deleted children are excluded, matching what a fresh
// aggregate would return.
//
// A rollup requires an active transaction on the child write — otherwise a child
// insert could commit while the parent update fails, leaving exactly the drift
// it exists to prevent. Register maniflex.WithTransaction on the Service step for
// the child's create/update/delete; without one the write is refused with
// 500 ROLLUP_NO_TX, following the mfx:"lock_scope" precedent.
//
// Field names are JSON names, resolved to columns and validated at registration,
// so a typo is a startup error naming the field — not a tag mini-language that
// fails silently at runtime.
type Rollup struct {
	// Parent is the model carrying the denormalised column.
	Parent string
	// ParentField is the JSON name of that column.
	ParentField string
	// Op is the aggregate: AggSum, AggCount, AggAvg, AggMin or AggMax.
	Op AggregateOp
	// Child is the model whose rows are aggregated.
	Child string
	// ChildField is the JSON name of the aggregated column. Required for every
	// op except AggCount, which counts rows.
	ChildField string
	// On is the JSON name of the foreign key on Child that points to Parent's id.
	On string
}

// compiledRollup is a Rollup with its names resolved to DB columns and its
// models bound, built once at registration.
type compiledRollup struct {
	cfg           Rollup
	parentFieldDB string
	childFieldDB  string
	onDB          string
	onJSON        string
	// softDelete, when non-nil, excludes soft-deleted children from the recompute.
	softDelete *FilterExpr
}

// rollupOps is the set of aggregates a rollup may maintain. AggCountDistinct is
// excluded: it needs a field and a distinctness key that the Rollup shape does
// not express, and a rollup over it is better written by hand.
var rollupOps = map[AggregateOp]bool{
	AggSum: true, AggCount: true, AggAvg: true, AggMin: true, AggMax: true,
}

// RegisterRollup installs a maintained aggregate column. It validates the
// configuration against the registry and wires a DB-step middleware on the child
// model for create, update and delete. Must be called before Start()/Handler().
//
// Returns an error rather than panicking so the configuration can be validated
// in a test; MustRegisterRollup is the panic-on-error variant for main().
func (s *Server) RegisterRollup(r Rollup) error {
	if s.sealed() {
		return fmt.Errorf("maniflex: RegisterRollup must be called before Start() or Handler()")
	}
	cr, err := s.compileRollup(r)
	if err != nil {
		return err
	}
	s.rollups = append(s.rollups, cr)
	s.Pipeline.DB.Register(
		cr.middleware(),
		ForModel(r.Child),
		ForOperation(OpCreate, OpUpdate, OpDelete),
	)
	return nil
}

// MustRegisterRollup is the panic-on-error variant of RegisterRollup, for use in
// main() or package initialisation.
func (s *Server) MustRegisterRollup(r Rollup) {
	if err := s.RegisterRollup(r); err != nil {
		panic(err)
	}
}

// compileRollup resolves and validates a Rollup, failing fast on anything a tag
// mini-language would have discovered only at runtime.
func (s *Server) compileRollup(r Rollup) (compiledRollup, error) {
	if r.Parent == "" || r.Child == "" || r.ParentField == "" || r.On == "" {
		return compiledRollup{}, fmt.Errorf(
			"maniflex: Rollup requires Parent, ParentField, Child and On")
	}
	if !rollupOps[r.Op] {
		return compiledRollup{}, fmt.Errorf(
			"maniflex: Rollup.Op %q is not a supported aggregate (AggSum, AggCount, AggAvg, AggMin, AggMax)", r.Op)
	}
	if r.Op != AggCount && r.ChildField == "" {
		return compiledRollup{}, fmt.Errorf(
			"maniflex: Rollup with Op %q requires ChildField (only AggCount may omit it)", r.Op)
	}

	parent, ok := s.registry.Get(r.Parent)
	if !ok {
		return compiledRollup{}, fmt.Errorf("maniflex: Rollup parent model %q is not registered", r.Parent)
	}
	child, ok := s.registry.Get(r.Child)
	if !ok {
		return compiledRollup{}, fmt.Errorf("maniflex: Rollup child model %q is not registered", r.Child)
	}

	parentField := parent.FieldByJSONName(r.ParentField)
	if parentField == nil {
		return compiledRollup{}, fmt.Errorf(
			"maniflex: Rollup ParentField %q does not exist on model %q", r.ParentField, r.Parent)
	}
	onField := child.FieldByJSONName(r.On)
	if onField == nil {
		return compiledRollup{}, fmt.Errorf(
			"maniflex: Rollup On field %q does not exist on model %q", r.On, r.Child)
	}
	cr := compiledRollup{
		cfg:           r,
		parentFieldDB: parentField.Tags.DBName,
		onDB:          onField.Tags.DBName,
		onJSON:        r.On,
	}
	if r.Op != AggCount {
		childField := child.FieldByJSONName(r.ChildField)
		if childField == nil {
			return compiledRollup{}, fmt.Errorf(
				"maniflex: Rollup ChildField %q does not exist on model %q", r.ChildField, r.Child)
		}
		cr.childFieldDB = childField.Tags.DBName
	}
	if sd := child.SoftDelete; sd.Enabled {
		cr.softDelete = softDeleteFilter(sd)
	}
	return cr, nil
}

// softDeleteFilter builds the FilterExpr that keeps only live rows for a
// soft-deletable model, so a recompute excludes soft-deleted children.
func softDeleteFilter(sd SoftDeleteConfig) *FilterExpr {
	if sd.FieldType == SoftDeleteBool {
		return &FilterExpr{Field: sd.Field, Operator: OpEq, Value: false}
	}
	return &FilterExpr{Field: sd.Field, Operator: OpIsNull}
}

// middleware returns the DB-step middleware that recomputes the affected parents
// after a child write.
func (cr compiledRollup) middleware() MiddlewareFunc {
	return func(ctx *ServerContext, next func() error) error {
		// Which parents this write touches, read before the write runs so a
		// delete (row gone afterwards) and a re-parenting update (old FK gone
		// from the body) are both captured.
		affected := cr.affectedParents(ctx)

		if len(affected) > 0 && ctx.Tx == nil {
			ctx.Abort(500, "ROLLUP_NO_TX",
				fmt.Sprintf("rollup %s.%s requires an active transaction; register "+
					"maniflex.WithTransaction(nil) on the Service step for %s writes",
					cr.cfg.Parent, cr.cfg.ParentField, cr.cfg.Child))
			return nil
		}

		if err := next(); err != nil {
			return err
		}
		// The write was rejected downstream — nothing changed, nothing to recompute.
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}

		for parentID := range affected {
			if err := cr.recompute(ctx, parentID); err != nil {
				ctx.Abort(500, "ROLLUP_ERROR",
					fmt.Sprintf("failed to recompute rollup %s.%s: %v",
						cr.cfg.Parent, cr.cfg.ParentField, err))
				return nil
			}
		}
		return nil
	}
}

// affectedParents returns the ids of the parents whose rollup this write can
// change: the pre-write parent (update, delete) and the body's parent (create,
// update). On a re-parenting update the two differ and both are returned.
func (cr compiledRollup) affectedParents(ctx *ServerContext) map[string]struct{} {
	out := make(map[string]struct{}, 2)

	if (ctx.Operation == OpUpdate || ctx.Operation == OpDelete) && ctx.ResourceID != "" {
		if old, err := ctx.GetModel(cr.cfg.Child).Read(ctx.ResourceID); err == nil {
			addParentID(out, old[cr.onDB])
		}
	}
	if (ctx.Operation == OpCreate || ctx.Operation == OpUpdate) && ctx.ParsedBody != nil {
		if v, present := ctx.ParsedBody.Get(cr.onJSON); present {
			addParentID(out, v)
		}
	}
	return out
}

// addParentID adds a non-empty parent id to the set.
func addParentID(set map[string]struct{}, v any) {
	if v == nil {
		return
	}
	id := fmt.Sprint(v)
	if id != "" {
		set[id] = struct{}{}
	}
}

// recompute recomputes the aggregate over parentID's live children and writes it
// to the parent's rollup column, through ctx.Tx when one is active.
func (cr compiledRollup) recompute(ctx *ServerContext, parentID string) error {
	where := []*FilterExpr{{Field: cr.onDB, Operator: OpEq, Value: parentID}}
	if cr.softDelete != nil {
		where = append(where, cr.softDelete)
	}
	rows, err := ctx.Aggregate(cr.cfg.Child, AggregateQuery{
		Select: []AggregateField{{Op: cr.cfg.Op, Field: cr.childFieldDB, As: "v"}},
		Where:  where,
	})
	if err != nil {
		return err
	}

	value := cr.emptyValue()
	if len(rows) > 0 && rows[0]["v"] != nil {
		value = rows[0]["v"]
	}

	_, err = ctx.GetModel(cr.cfg.Parent).Update(parentID, map[string]any{cr.parentFieldDB: value})
	return err
}

// emptyValue is what the parent column holds when the parent has no live
// children. A sum or count of nothing is 0; a min/max/avg of nothing is NULL,
// which is what the aggregate itself returns and the only honest answer.
func (cr compiledRollup) emptyValue() any {
	switch cr.cfg.Op {
	case AggSum, AggCount:
		return int64(0)
	default:
		return nil
	}
}

// BackfillRollups recomputes every registered rollup for every parent, from the
// current child rows. Run it once after adding a rollup to an existing dataset,
// or to reconcile a column edited out of band; the live middleware keeps it exact
// thereafter.
//
// It reconciles rather than locks: each parent is recomputed independently
// against the child rows as they stand, so a concurrent write during the backfill
// is simply picked up by that write's own rollup. Prefer a quiet window for large
// tables all the same.
func (s *Server) BackfillRollups(ctx context.Context) error {
	if len(s.rollups) == 0 {
		return nil
	}
	bg := NewBackground(ctx, s.cfg.DB, s.registry)
	for _, cr := range s.rollups {
		if err := cr.backfill(bg); err != nil {
			return fmt.Errorf("maniflex: backfill rollup %s.%s: %w",
				cr.cfg.Parent, cr.cfg.ParentField, err)
		}
	}
	return nil
}

// backfill recomputes this rollup for every parent that has a child row.
func (cr compiledRollup) backfill(ctx *ServerContext) error {
	child, ok := ctx.reg.Get(cr.cfg.Child)
	if !ok {
		return fmt.Errorf("child model %q is not registered", cr.cfg.Child)
	}
	// DISTINCT over the FK column: the identifiers are validated DB names from
	// the registry, so the statement is not client-influenced.
	q := fmt.Sprintf("SELECT DISTINCT %s AS pid FROM %s", cr.onDB, child.TableName)
	rows, err := ctx.rawQuery(q)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row["pid"] == nil {
			continue
		}
		if err := cr.recompute(ctx, fmt.Sprint(row["pid"])); err != nil {
			return err
		}
	}
	return nil
}
