package maniflex

import (
	"fmt"
	"strings"
)

// RecursiveDirection controls which direction a recursive CTE traversal walks.
type RecursiveDirection string

const (
	// RecursiveDescendants walks the tree downward, collecting all children of
	// the root node. This is the default when Direction is unset.
	RecursiveDescendants RecursiveDirection = "descendants"
	// RecursiveAncestors walks the tree upward, collecting all parents of the
	// starting node up to the root.
	RecursiveAncestors RecursiveDirection = "ancestors"
)

// DefaultRecursiveMaxDepth bounds a RecursiveQuery whose MaxDepth is left at
// the zero value. Before v0.2.5 the zero value meant unlimited, which made an
// unbounded traversal the default rather than a deliberate choice.
const DefaultRecursiveMaxDepth = 100

// recursivePathCol is the CTE column carrying the ids visited so far, used for
// cycle detection. It is stripped from the returned rows.
const recursivePathCol = "_path"

// RecursiveQuery is the input to ctx.RecursiveQuery.
type RecursiveQuery struct {
	// RootID is the id of the starting node. Required.
	RootID string
	// ParentField is the DB column that holds the parent's id.
	// e.g. "parent_id", "manager_id". Required; must be a registered column.
	ParentField string
	// MaxDepth limits how many levels to traverse. MaxDepth=1 returns the root
	// plus its immediate children/parent only.
	//
	// 0 (the zero value) applies DefaultRecursiveMaxDepth. A negative value
	// means genuinely unlimited — say so explicitly if you want it, because an
	// unbounded traversal of a large hierarchy is a request that never ends.
	MaxDepth int
	// Direction controls descent (Descendants, default) or ascent (Ancestors).
	Direction RecursiveDirection
	// Where are additional filters applied in both the anchor and recursive
	// members. Nested-relation filters are not supported and return an error.
	Where []*FilterExpr
}

// RecursiveQuery executes a WITH RECURSIVE CTE against a self-referential
// model and returns all nodes in the subtree (or ancestor chain) starting at
// q.RootID. Every returned row contains an integer _depth column (0 = root,
// 1 = immediate children/parent, …). Rows are ordered by _depth ascending.
//
// Both Postgres (WITH RECURSIVE … $N) and SQLite (WITH RECURSIVE … ?,
// supported since 3.8.3) are handled transparently.
//
// The query participates in ctx.Tx when one is active; otherwise it uses the
// adapter's read pool.
//
//	rows, err := ctx.RecursiveQuery("Category", maniflex.RecursiveQuery{
//	    RootID:      "some-uuid",
//	    ParentField: "parent_id",
//	    MaxDepth:    5,
//	})
//	// rows[0]["_depth"] == int64(0) is the root; rows[1..n] are descendants.
func (c *ServerContext) RecursiveQuery(modelName string, q RecursiveQuery) ([]Row, error) {
	// A recursive CTE walks the tree from an anchor row outwards; a scope filter
	// applied to it would either be wrong (dropping the anchor's descendants that
	// happen to sit outside the scope, silently truncating the tree) or
	// misleading (applied only to the anchor). Refuse rather than guess.
	if err := c.guardRaw("RecursiveQuery()"); err != nil {
		return nil, err
	}
	meta, err := rqValidate(c, modelName, q)
	if err != nil {
		return nil, err
	}

	dir := q.Direction
	if dir == "" {
		dir = RecursiveDescendants
	}

	driver := c.DriverType()
	pb := newAggPH(driver)
	table := aggQuote(meta.TableName)

	anchorWhere, recursiveWhere, err := rqBuildWhere(meta, q, table, driver, pb)
	if err != nil {
		return nil, err
	}

	joinCond := rqJoinCond(dir, table, q.ParentField)
	id := rqIDText(table)

	query := fmt.Sprintf(
		"WITH RECURSIVE _cte AS ("+
			"SELECT %s.*, 0 AS _depth, '/' || %s || '/' AS %s FROM %s%s"+
			" UNION ALL "+
			"SELECT %s.*, _cte._depth + 1 AS _depth, _cte.%s || %s || '/' AS %s"+
			" FROM %s JOIN _cte ON %s%s"+
			") SELECT * FROM _cte ORDER BY _depth",
		table, id, recursivePathCol, table, anchorWhere,
		table, recursivePathCol, id, recursivePathCol,
		table, joinCond, recursiveWhere,
	)

	// rawQuery: the scope guard above has already had its say on this call.
	rows, err := c.rawQuery(query, pb.args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		delete(r, recursivePathCol)
	}
	if rows == nil {
		return []map[string]any{}, nil
	}
	return rows, nil
}

// rqIDText renders the row's id as text, for building the visited-path string.
func rqIDText(tableQuoted string) string {
	return "CAST(" + tableQuoted + "." + aggQuote("id") + " AS TEXT)"
}

// rqCycleCond returns a predicate that admits a row only when its id is not
// already on the path walked to reach it, which is what stops a cyclic parent
// chain (a row that is its own ancestor) from looping forever.
//
// The test is an exact substring search rather than LIKE: an id is application
// data and may legitimately contain '%' or '_', which LIKE would treat as
// wildcards — '_' in particular would silently prune a real subtree by matching
// an unrelated id of the same length.
func rqCycleCond(tableQuoted string, driver DriverType) string {
	needle := "'/' || " + rqIDText(tableQuoted) + " || '/'"
	fn := "instr"
	if driver == Postgres {
		fn = "strpos"
	}
	return fn + "(_cte." + recursivePathCol + ", " + needle + ") = 0"
}

// rqValidate checks required fields and returns the resolved ModelMeta.
func rqValidate(c *ServerContext, modelName string, q RecursiveQuery) (*ModelMeta, error) {
	if c.reg == nil {
		return nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}
	meta, ok := c.reg.Get(modelName)
	if !ok {
		return nil, fmt.Errorf("maniflex: model %q is not registered", modelName)
	}
	if q.RootID == "" {
		return nil, fmt.Errorf("maniflex: RecursiveQuery.RootID is required")
	}
	if q.ParentField == "" {
		return nil, fmt.Errorf("maniflex: RecursiveQuery.ParentField is required")
	}
	if meta.FieldByDBName(q.ParentField) == nil {
		return nil, fmt.Errorf("maniflex: ParentField %q does not exist on model %q", q.ParentField, modelName)
	}
	for _, f := range q.Where {
		if !f.IsNested && meta.FieldByDBName(f.Field) == nil {
			return nil, fmt.Errorf("maniflex: Where filter references unknown column %q", f.Field)
		}
	}
	return meta, nil
}

// rqBuildWhere constructs the anchor and recursive-member WHERE clauses.
// aggBuildWhere is called twice so placeholder positions advance correctly;
// filter arg values are duplicated in pb.args — correct for both drivers.
func rqBuildWhere(meta *ModelMeta, q RecursiveQuery, table string, driver DriverType, pb *aggPH) (anchorWhere, recursiveWhere string, err error) {
	anchorConds := []string{table + "." + aggQuote("id") + " = " + pb.add(q.RootID)}
	if meta.SoftDelete.Enabled {
		anchorConds = append(anchorConds, rqSDCond(meta, table))
	}
	if filterSQL, ferr := aggBuildWhere(meta, q.Where, driver, pb); ferr != nil {
		return "", "", ferr
	} else if filterSQL != "" {
		anchorConds = append(anchorConds, strings.TrimPrefix(filterSQL, " WHERE "))
	}

	var recursiveConds []string
	if meta.SoftDelete.Enabled {
		recursiveConds = append(recursiveConds, rqSDCond(meta, table))
	}
	if filterSQL, ferr := aggBuildWhere(meta, q.Where, driver, pb); ferr != nil {
		return "", "", ferr
	} else if filterSQL != "" {
		recursiveConds = append(recursiveConds, strings.TrimPrefix(filterSQL, " WHERE "))
	}
	recursiveConds = append(recursiveConds, rqCycleCond(table, driver))
	if depth := rqEffectiveDepth(q.MaxDepth); depth > 0 {
		// Allow depths 0…depth inclusive: add a child only when parent depth < depth.
		recursiveConds = append(recursiveConds, "_cte._depth < "+pb.add(depth))
	}

	anchorWhere = " WHERE " + strings.Join(anchorConds, " AND ")
	if len(recursiveConds) > 0 {
		recursiveWhere = " WHERE " + strings.Join(recursiveConds, " AND ")
	}
	return anchorWhere, recursiveWhere, nil
}

// rqEffectiveDepth resolves the caller's MaxDepth: 0 takes the default cap,
// negative means unlimited (reported as 0, which emits no depth guard).
func rqEffectiveDepth(maxDepth int) int {
	switch {
	case maxDepth == 0:
		return DefaultRecursiveMaxDepth
	case maxDepth < 0:
		return 0
	}
	return maxDepth
}

// rqJoinCond returns the ON expression for the recursive member JOIN.
func rqJoinCond(dir RecursiveDirection, table, parentField string) string {
	if dir == RecursiveDescendants {
		return table + "." + aggQuote(parentField) + " = _cte." + aggQuote("id")
	}
	return table + "." + aggQuote("id") + " = _cte." + aggQuote(parentField)
}

// rqSDCond returns a soft-delete predicate qualified with the table name.
func rqSDCond(meta *ModelMeta, tableQuoted string) string {
	sd := meta.SoftDelete
	col := tableQuoted + "." + aggQuote(sd.Field)
	if sd.FieldType == SoftDeleteBool {
		return col + " = FALSE"
	}
	return col + " IS NULL"
}
