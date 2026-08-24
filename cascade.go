package maniflex

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
// constraint's ON DELETE clause. That is possible only when neither side
// soft-deletes: a soft delete is an UPDATE, so the constraint never fires on the
// parent, and a DB cascade can only hard-delete, so it cannot honour a
// soft-delete child. Every edge touching soft-delete is enforced in the maniflex
// layer instead.
func dbEnforcedDelete(parent, child *ModelMeta) bool {
	return !parent.SoftDelete.Enabled && !child.SoftDelete.Enabled
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

	visited := map[string]bool{cascadeKey(model.Name, ctx.ResourceID): true}
	if err := s.cascadeChildren(ctx, exec, model, ctx.ResourceID, visited); err != nil {
		if errors.Is(err, errCascadeRestricted) {
			return exec, own, nil // ctx.Response carries the 409; the DB step rolls back and sends it
		}
		return exec, own, err
	}
	return exec, own, nil
}

// cascadeChildren applies every onDelete edge pointing at parentModel/parentID:
// restrict refuses the delete (409), setNull nulls the child's FK, and cascade
// deletes the child through the adapter's own Delete — so a soft-delete child is
// soft-deleted identically to its parent — after recursing into that child's own
// children first. The visited set breaks reference cycles.
func (s *defaultSteps) cascadeChildren(ctx *ServerContext, exec dbExec, parentModel *ModelMeta, parentID string, visited map[string]bool) error {
	for _, edge := range childCascadeEdges(s.reg, parentModel.Name) {
		// A hard-delete/hard-delete edge is enforced by the database's own FK
		// constraint (ForeignKeysFor emits it), so leave it to the DB — handling it
		// here too would delete the children twice over.
		if dbEnforcedDelete(parentModel, edge.child) {
			continue
		}
		if err := s.applyCascadeEdge(ctx, exec, parentModel, edge, parentID, visited); err != nil {
			return err
		}
	}
	return nil
}

// applyCascadeEdge carries out one edge's onDelete action against the child rows
// that reference the parent.
func (s *defaultSteps) applyCascadeEdge(ctx *ServerContext, exec dbExec, parentModel *ModelMeta, edge cascadeEdge, parentID string, visited map[string]bool) error {
	switch edge.rel.OnDelete {
	case OnDeleteRestrict:
		return s.cascadeRestrict(ctx, exec, parentModel, edge, parentID)
	case OnDeleteSetNull:
		return s.cascadeSetNull(ctx, exec, edge.child, edge.rel.FKColumn, parentID)
	case OnDeleteCascade:
		return s.cascadeDeleteRows(ctx, exec, edge.child, edge.rel.FKColumn, parentID, visited)
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
func (s *defaultSteps) cascadeSetNull(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID string) error {
	return s.eachChildID(ctx, exec, child, fkCol, parentID, func(id string) error {
		_, err := exec.Update(ctx.Ctx, child, id, map[string]any{fkCol: nil})
		return err
	})
}

// cascadeDeleteRows deletes each referencing child through the adapter's own
// Delete — so a soft-delete child is soft-deleted — after recursing into that
// child's own children first, so a child is never deleted while its children
// still point at it. The visited set breaks reference cycles.
func (s *defaultSteps) cascadeDeleteRows(ctx *ServerContext, exec dbExec, child *ModelMeta, fkCol, parentID string, visited map[string]bool) error {
	return s.eachChildID(ctx, exec, child, fkCol, parentID, func(id string) error {
		key := cascadeKey(child.Name, id)
		if visited[key] {
			return nil // already being deleted in this sweep — a cycle
		}
		visited[key] = true
		if err := s.cascadeChildren(ctx, exec, child, id, visited); err != nil {
			return err
		}
		return exec.Delete(ctx.Ctx, child, id)
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
