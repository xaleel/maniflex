package maniflex

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
)

// errCascadeRestricted is the sentinel a restrict edge raises to unwind the
// recursion. ctx.Response already carries the 409; the DB step rolls the
// transaction back and sends it.
var errCascadeRestricted = errors.New("cascade: delete restricted by a child relation")

// Cascading deletes (5.16). The mfx:"relation:Parent;onDelete:ACTION" tag lives
// on the child's FK (Post.AuthorID → Author), so acting on it when an Author is
// deleted is a reverse lookup across every registered model. The action is
// enforced two ways, split by soft-delete:
//
//   - When neither the parent nor the child soft-deletes, a real database FK
//     constraint carries the ON DELETE clause and the database enforces it (see
//     the migrator). A hard DELETE of the parent cascades/nulls/restricts natively.
//   - When either side soft-deletes, the database cannot help — a soft delete is
//     an UPDATE, so an ON DELETE clause never fires, and a DB cascade can only
//     hard-delete, so it cannot soft-delete a child. Those edges are enforced in
//     the maniflex delete path instead, in the parent delete's own transaction.
//
// dbEnforcedDelete draws that line; childCascadeEdges finds the edges.

// cascadeEdge is one child relation that must react to the deletion of a parent
// row: the child model, and the BelongsTo relation on it whose onDelete action
// points back at the parent.
type cascadeEdge struct {
	child *ModelMeta
	rel   RelationMeta
}

// childCascadeEdges returns every registered relation that declares an onDelete
// action against parentModelName — the children whose rows must be cascaded,
// nulled, or protected when a row of the parent is deleted.
func childCascadeEdges(reg RegistryAccessor, parentModelName string) []cascadeEdge {
	var out []cascadeEdge
	for _, m := range reg.All() {
		for _, rel := range m.Relations {
			if rel.Kind == BelongsTo && rel.OnDelete != OnDeleteNoAction && rel.RelatedModel == parentModelName {
				out = append(out, cascadeEdge{child: m, rel: rel})
			}
		}
	}
	return out
}

// dbEnforcedDelete reports whether an edge can be left to a database FK
// constraint's ON DELETE clause.
//
// Soft-delete rules one side out: a soft delete is an UPDATE, so the constraint
// never fires on the parent, and a DB cascade can only hard-delete, so it cannot
// honour a soft-delete child.
//
// Bookkeeping rules out the other. A database ON DELETE clause removes the rows
// and tells nobody — which is exactly what a versioned child or a rollup child
// cannot afford. The history gains no delete row, so the audit trail has holes
// precisely on bulk destructive operations and VersionedRequired's fail-closed
// contract goes unhonoured; the rollup keeps counting children that are gone,
// drifting from a total its own documentation calls correct by construction, and
// curable only by BackfillRollups (audit PIPE-4). Those edges go through the
// maniflex walk, which is the only place the child's bookkeeping can run.
//
// That is the whole rule: the framework enforces an edge itself whenever it has
// something to do beyond deleting the row.
func dbEnforcedDelete(parent, child *ModelMeta) bool {
	if parent.SoftDelete.Enabled || child.SoftDelete.Enabled {
		return false
	}
	return !child.Config.Versioned && !child.rollupChild
}

// ForeignKeySpec describes a foreign-key constraint an adapter should emit for a
// model. It is derived from an mfx:"relation:Parent;onDelete:ACTION" tag whose
// action the database can enforce — one where neither side soft-deletes. The FK
// lives on the child table (Column); RefTable/RefColumn name the parent.
type ForeignKeySpec struct {
	Name      string         // deterministic constraint name
	Column    string         // FK column on this (child) table
	RefTable  string         // referenced (parent) table
	RefColumn string         // referenced (parent) column — its primary key
	OnDelete  OnDeleteAction // cascade / setNull / restrict
}

// ForeignKeysFor returns the FK constraints an adapter should emit for model m:
// one per BelongsTo relation carrying an onDelete action the database can enforce
// (dbEnforcedDelete). Edges that touch soft-delete are enforced in the maniflex
// delete path instead and are not returned here — so the FK constraints an
// adapter emits and the edges enforceCascadeDelete skips are the same set, drawn
// by the same line.
func ForeignKeysFor(reg RegistryAccessor, m *ModelMeta) []ForeignKeySpec {
	junction := isJunction(m)
	var out []ForeignKeySpec
	for _, rel := range m.Relations {
		if rel.Kind != BelongsTo {
			continue
		}
		action := rel.OnDelete
		if action == OnDeleteNoAction && junction {
			// A junction's keys cascade unless the model says otherwise. A link
			// row pointing at an endpoint that no longer exists says nothing,
			// and keeping it is how join tables accumulated orphans that only a
			// manual sweep removed (audit MS-L10). An explicit mfx:"on_delete:"
			// on the column still wins — this only fills the unset case.
			action = OnDeleteCascade
		}
		if action == OnDeleteNoAction {
			continue
		}
		parent, ok := reg.Get(rel.RelatedModel)
		if !ok || !dbEnforcedDelete(parent, m) {
			continue
		}
		out = append(out, ForeignKeySpec{
			Name:      fmt.Sprintf("fk_%s_%s", m.TableName, rel.FKColumn),
			Column:    rel.FKColumn,
			RefTable:  parent.TableName,
			RefColumn: "id",
			OnDelete:  action,
		})
	}
	return out
}

// validateOnDeleteActions checks, once all models are registered, that every
// onDelete action can actually be enforced: its target model is known, and
// setNull targets a nullable FK. A directive that parses but cannot be enforced
// is a registration error, not a silent no-op — the rule the recent releases set
// for every other unenforceable-looking tag.
func validateOnDeleteActions(reg RegistryAccessor) error {
	for _, m := range reg.All() {
		for _, rel := range m.Relations {
			if rel.Kind != BelongsTo || rel.OnDelete == OnDeleteNoAction {
				continue
			}
			if err := validateOnDeleteEdge(reg, m, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateOnDeleteEdge checks one onDelete relation: its target is registered,
// and setNull targets a nullable FK.
func validateOnDeleteEdge(reg RegistryAccessor, m *ModelMeta, rel RelationMeta) error {
	if _, ok := reg.Get(rel.RelatedModel); !ok {
		return fmt.Errorf(
			"maniflex: model %q relation %q declares onDelete:%s but its target model %q is not "+
				"registered — an onDelete action needs a known model to act on. Register %q, or drop the onDelete",
			m.Name, rel.RelationKey, rel.OnDelete, rel.RelatedModel, rel.RelatedModel)
	}
	if rel.OnDelete == OnDeleteSetNull {
		f := m.FieldByDBName(rel.FKColumn)
		if f == nil || f.Type.Kind() != reflect.Pointer {
			return fmt.Errorf(
				"maniflex: model %q relation %q is onDelete:setNull but its FK column %q is NOT NULL — "+
					"setNull writes NULL into it when the parent is deleted, which the column forbids. Make the "+
					"FK a pointer (e.g. *string) so it is nullable, or use onDelete:cascade / onDelete:restrict",
				m.Name, rel.RelationKey, rel.FKColumn)
		}
	}
	return nil
}

// enforceCascadeDelete applies onDelete actions to a parent's children before the
// parent row itself is deleted, in the parent delete's own transaction so the
// whole deletion is atomic. It runs only for OpDelete, and only when some model
// declares an onDelete against this one. It returns the exec the rest of the DB
// step must use and the transaction it owns (nil when it joined one an earlier
// guard opened).
//
// Every edge is handled in the maniflex layer today. Once the migrator emits real
// FK constraints (Phase 3), the hard-delete/hard-delete edges — the ones
// dbEnforcedDelete reports true for — are left to the database and skipped here.
func (s *defaultSteps) enforceCascadeDelete(ctx *ServerContext, exec dbExec, model *ModelMeta) (dbExec, Tx, error) {
	if ctx.Operation != OpDelete {
		return exec, nil, nil
	}
	if len(childCascadeEdges(s.reg, model.Name)) == 0 {
		return exec, nil, nil // nothing references this model — no read, no transaction
	}

	exec, own, err := s.ensureScopeTx(ctx, exec)
	if err != nil {
		return exec, own, err
	}

	sweep := &cascadeSweep{
		visited: map[string]bool{cascadeKey(model.Name, ctx.ResourceID): true},
	}
	if err := s.cascadeChildren(ctx, exec, model, ctx.ResourceID, sweep); err != nil {
		if errors.Is(err, errCascadeRestricted) {
			return exec, own, nil // ctx.Response carries the 409; the DB step rolls back and sends it
		}
		return exec, own, err
	}
	if err := s.runPendingRollups(ctx, sweep); err != nil {
		return exec, own, err
	}
	return exec, own, nil
}

// cascadeSweep is the state one delete's cascade carries across its recursion:
// the cycle guard, and the rollup recomputes the sweep has earned but not yet
// performed.
type cascadeSweep struct {
	visited map[string]bool
	pending map[string]pendingRollup
}

// pendingRollup is one parent column the sweep has invalidated.
type pendingRollup struct {
	rollup   compiledRollup
	parentID string
}

// invalidate notes that a row this rollup summarises has just been removed from
// the aggregate, so its parent's column no longer matches the rows beneath it.
// parentVal is the foreign key read off the child before the write.
func (s *cascadeSweep) invalidate(cr compiledRollup, parentVal any) {
	id := foreignKeyID(parentVal)
	if id == "" {
		return
	}
	if s.pending == nil {
		s.pending = make(map[string]pendingRollup, 1)
	}
	// Keyed by the column, not just the parent: two rollups can maintain
	// different columns of the same parent from the same child.
	s.pending[cr.cfg.Parent+"\x00"+cr.parentFieldDB+"\x00"+id] = pendingRollup{rollup: cr, parentID: id}
}

// runPendingRollups recomputes every rollup column the sweep disturbed, once
// each and in a fixed order.
//
// Once each because a parent with a hundred cascaded children needs one
// recompute, not a hundred: each one aggregates the child rows as they now
// stand, so the last would be the only one that counted. In a fixed order
// because recompute takes the parent's row lock, and two concurrent deletes
// reaching the same two parents in opposite orders would deadlock — the same
// reason affectedParents sorts its ids.
//
// A parent the sweep itself deleted is skipped rather than failed: a chain like
// Author → Post → Comment, where Comment rolls up into Post, invalidates a Post
// that is gone by the time the walk ends. There is no column left to correct.
func (s *defaultSteps) runPendingRollups(ctx *ServerContext, sweep *cascadeSweep) error {
	for _, key := range slices.Sorted(maps.Keys(sweep.pending)) {
		p := sweep.pending[key]
		if err := p.rollup.recompute(ctx, p.parentID); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return fmt.Errorf("cascade: recompute rollup %s.%s: %w",
				p.rollup.cfg.Parent, p.rollup.cfg.ParentField, err)
		}
	}
	return nil
}

// cascadeHooks is what the framework owes a cascaded child beyond the row write:
// a history row when the child is versioned, and a recompute of every rollup
// that summarises it.
//
// Empty for an ordinary child, which is the common case and pays nothing — not
// even the row read, since childIDPage selects only the id.
type cascadeHooks struct {
	histMeta *ModelMeta       // nil unless the child is versioned
	rollups  []compiledRollup // rollups whose Child is this model
}

func (h cascadeHooks) none() bool { return h.histMeta == nil && len(h.rollups) == 0 }

func (s *defaultSteps) cascadeHooksFor(child *ModelMeta) cascadeHooks {
	var h cascadeHooks
	if child.Config.Versioned {
		if hm, ok := s.reg.Get(child.Name + "History"); ok {
			h.histMeta = hm
		}
	}
	for _, cr := range s.rollups {
		if cr.cfg.Child == child.Name {
			h.rollups = append(h.rollups, cr)
		}
	}
	return h
}

// cascadePreImage reads the row a cascade is about to write, when anything needs
// it: the history row's diff and snapshot, and the foreign key naming the rollup
// parent to recompute. One read serves both, and a child with neither pays none.
func (s *defaultSteps) cascadePreImage(ctx *ServerContext, exec dbExec, child *ModelMeta,
	id string, hooks cascadeHooks,
) (map[string]any, error) {
	if hooks.none() {
		return nil, nil
	}
	return exec.FindByID(ctx.Ctx, child, id, &QueryParams{Page: 1, Limit: 1})
}

// afterCascadeWrite runs the bookkeeping the child's own DB step would have, had
// the cascade gone through it. It does not: the cascade writes children with the
// adapter directly, which is what makes it one data operation rather than N
// requests, and is also why none of this happened at all (audit PIPE-4).
func (s *defaultSteps) afterCascadeWrite(ctx *ServerContext, exec dbExec, child *ModelMeta,
	hooks cascadeHooks, op Operation, id string, pre, post map[string]any, sweep *cascadeSweep,
) error {
	if pre != nil {
		for _, cr := range hooks.rollups {
			sweep.invalidate(cr, pre[cr.onDB])
		}
	}
	if hooks.histMeta == nil {
		return nil
	}
	if err := writeHistoryRow(ctx, exec, child, hooks.histMeta, op, id, pre, post); err != nil {
		ctx.Logger().Error("versioning: cascade history write failed",
			"model", child.Name, "record_id", id, "error", err)
		// Fail-closed when the model asks for it, exactly as the DB-step writer
		// does: returning the error rolls back the parent's delete, so the
		// cascade and its history stand or fall together.
		if child.Config.VersionedRequired {
			return fmt.Errorf("versioning: cascade history write failed for %s: %w", child.Name, err)
		}
	}
	return nil
}

// cascadeChildren applies every onDelete edge pointing at parentModel/parentID:
// restrict refuses the delete (409), setNull nulls the child's FK, and cascade
// deletes the child through the adapter's own Delete — so a soft-delete child is
// soft-deleted identically to its parent — after recursing into that child's own
// children first. The visited set breaks reference cycles.
func (s *defaultSteps) cascadeChildren(ctx *ServerContext, exec dbExec, parentModel *ModelMeta, parentID string, sweep *cascadeSweep) error {
	for _, edge := range childCascadeEdges(s.reg, parentModel.Name) {
		// An edge the database enforces with its own FK constraint
		// (ForeignKeysFor emits it) is left to the DB — handling it here too
		// would delete the children twice over.
		if dbEnforcedDelete(parentModel, edge.child) {
			continue
		}
		if err := s.applyCascadeEdge(ctx, exec, parentModel, edge, parentID, sweep); err != nil {
			return err
		}
	}
	return nil
}

// applyCascadeEdge carries out one edge's onDelete action against the child rows
// that reference the parent.
func (s *defaultSteps) applyCascadeEdge(ctx *ServerContext, exec dbExec, parentModel *ModelMeta, edge cascadeEdge, parentID string, sweep *cascadeSweep) error {
	switch edge.rel.OnDelete {
	case OnDeleteRestrict:
		return s.cascadeRestrict(ctx, exec, parentModel, edge, parentID)
	case OnDeleteSetNull:
		return s.cascadeSetNull(ctx, exec, edge.child, edge.rel.FKColumn, parentID, sweep)
	case OnDeleteCascade:
		return s.cascadeDeleteRows(ctx, exec, edge.child, edge.rel.FKColumn, parentID, sweep)
	}
	return nil
}

// cascadeRestrict refuses the parent's deletion when any child still references
// it, reporting how many do.
//
// It asks for the count and a single row rather than for the rows: the message
// needs a number, and nothing else here looks at a child at all. Loading the
// fan-out to take its length was the most expensive way to answer the cheapest
// question on this path — and restrict is the edge where the fan-out is largest,
// since a table nobody may orphan is a table rows accumulate in (audit O9).
//
// The refusal turns on whether a row came back, not on the count, so an adapter
// whose FindMany does not report a total cannot make the guard fail open.
func (s *defaultSteps) cascadeRestrict(ctx *ServerContext, exec dbExec, parentModel *ModelMeta, edge cascadeEdge, parentID string) error {
	rows, total, err := exec.FindMany(ctx.Ctx, edge.child, &QueryParams{
		Page:    1,
		Limit:   1,
		Fields:  []string{"id"},
		Filters: []*FilterExpr{{Field: edge.rel.FKColumn, Operator: OpEq, Value: parentID}},
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if total < int64(len(rows)) {
		total = int64(len(rows))
	}
	ctx.Abort(http.StatusConflict, "DELETE_RESTRICTED", fmt.Sprintf(
		"%s cannot be deleted: %d %s record(s) still reference it (onDelete:restrict)",
		parentModel.Name, total, edge.child.Name))
	return errCascadeRestricted
}

// cascadeSetNull nulls the FK column of each referencing child row.
//
// The child survives, so this is an update as far as its bookkeeping is
// concerned: a versioned child gains an update row, and a rollup keyed on the
// very column being nulled loses this child from its old parent's total.
func (s *defaultSteps) cascadeSetNull(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID string, sweep *cascadeSweep) error {
	hooks := s.cascadeHooksFor(child)
	return s.eachChildID(ctx, exec, child, fkCol, parentID, func(id string) error {
		pre, err := s.cascadePreImage(ctx, exec, child, id, hooks)
		if err != nil {
			return err
		}
		post, err := exec.Update(ctx.Ctx, child, id, map[string]any{fkCol: nil})
		if err != nil {
			return err
		}
		return s.afterCascadeWrite(ctx, exec, child, hooks, OpUpdate, id, pre, post, sweep)
	})
}

// cascadeDeleteRows deletes each referencing child through the adapter's own
// Delete — so a soft-delete child is soft-deleted — after recursing into that
// child's own children first, so a child is never deleted while its children
// still point at it. The visited set breaks reference cycles.
func (s *defaultSteps) cascadeDeleteRows(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID string, sweep *cascadeSweep) error {
	hooks := s.cascadeHooksFor(child)
	return s.eachChildID(ctx, exec, child, fkCol, parentID, func(id string) error {
		key := cascadeKey(child.Name, id)
		if sweep.visited[key] {
			return nil // already being deleted in this sweep — a cycle
		}
		sweep.visited[key] = true
		if err := s.cascadeChildren(ctx, exec, child, id, sweep); err != nil {
			return err
		}
		// Read before the write: a deleted row has no pre-image to diff against,
		// and a hard-deleted one has no foreign key left to name its rollup parent.
		pre, err := s.cascadePreImage(ctx, exec, child, id, hooks)
		if err != nil {
			return err
		}
		if err := exec.Delete(ctx.Ctx, child, id); err != nil {
			return err
		}
		return s.afterCascadeWrite(ctx, exec, child, hooks, OpDelete, id, pre, nil, sweep)
	})
}

// cascadePageSize is how many child ids one edge reads at a time.
const cascadePageSize = 500

// eachChildID calls fn with the id of every child row whose FK column equals
// parentID, a page at a time.
//
// The walk is keyset, not offset: each page asks for the ids after the last one
// seen, ordered by id. Offset paging was wrong here twice over (audit O9).
//
// It was unordered, and a SELECT with no ORDER BY may return rows in any order —
// nothing obliges a database to choose the same one for the next OFFSET, and
// Postgres does not: it costs the offset into the plan, and a synchronised
// sequential scan joins whichever scan is already running. Rows either side of a
// page boundary were therefore skipped, and a child skipped on a cascade edge is
// one left pointing at a parent that no longer exists.
//
// And it could only be made safe by reading the whole fan-out before touching
// any of it, since deleting a row shifts every later offset. That snapshot held
// one full column map per child — of which nothing but the id was ever read — so
// the memory a delete needed was set by the size of the fan-out. Keyset paging
// needs no snapshot: the bound is a value, so removing a row already behind it
// moves nothing, and the walk holds one page.
func (s *defaultSteps) eachChildID(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID string, fn func(id string) error) error {
	lastID := ""
	for {
		ids, err := s.childIDPage(ctx, exec, child, fkCol, parentID, lastID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := fn(id); err != nil {
				return err
			}
		}
		lastID = ids[len(ids)-1]
	}
}

// childIDPage reads one page of child ids, ordered by id and bounded below by
// lastID. Only the id column is selected: it is the only thing any caller uses.
func (s *defaultSteps) childIDPage(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID, lastID string) ([]string, error) {
	q := &QueryParams{
		Page:    1,
		Limit:   cascadePageSize,
		Fields:  []string{"id"},
		Sorts:   []SortExpr{{DBName: "id", Direction: SortAsc}},
		Filters: []*FilterExpr{{Field: fkCol, Operator: OpEq, Value: parentID}},
	}
	if lastID != "" {
		q.Filters = append(q.Filters, &FilterExpr{Field: "id", Operator: OpGt, Value: lastID})
	}
	rows, _, err := exec.FindMany(ctx.Ctx, child, q)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if id := cascadeID(row); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// foreignKeyID renders a foreign-key value read off a record as the id it names.
//
// A nullable FK — the shape onDelete:setNull requires — reaches here as a
// *string, because recordToMap stores each struct field as it stands. fmt.Sprint
// on that yields a pointer address, which names no row at all.
func foreignKeyID(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return ""
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	return fmt.Sprint(rv.Interface())
}

// cascadeKey identifies a row across the cascade sweep, for the cycle guard.
func cascadeKey(model, id string) string { return model + "\x00" + id }

// cascadeID extracts a row's primary key as a string.
func cascadeID(row map[string]any) string {
	switch v := row["id"].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}
