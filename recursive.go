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

// RecursiveQuery is the input to ctx.RecursiveQuery.
type RecursiveQuery struct {
	// RootID is the id of the starting node. Required.
	RootID string
	// ParentField is the DB column that holds the parent's id.
	// e.g. "parent_id", "manager_id". Required; must be a registered column.
	ParentField string
	// MaxDepth limits how many levels to traverse. 0 means unlimited.
	// MaxDepth=1 returns the root plus its immediate children/parent only.
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

	query := fmt.Sprintf(
		"WITH RECURSIVE _cte AS ("+
			"SELECT %s.*, 0 AS _depth FROM %s%s"+
			" UNION ALL "+
			"SELECT %s.*, _cte._depth + 1 AS _depth FROM %s JOIN _cte ON %s%s"+
			") SELECT * FROM _cte ORDER BY _depth",
		table, table, anchorWhere,
		table, table, joinCond, recursiveWhere,
	)

	// rawQuery: the scope guard above has already had its say on this call.
	rows, err := c.rawQuery(query, pb.args...)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return []map[string]any{}, nil
	}
	return rows, nil
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
	if q.MaxDepth > 0 {
		// Allow depths 0…MaxDepth inclusive: add a child only when parent depth < MaxDepth.
		recursiveConds = append(recursiveConds, "_cte._depth < "+pb.add(q.MaxDepth))
	}

	anchorWhere = " WHERE " + strings.Join(anchorConds, " AND ")
	if len(recursiveConds) > 0 {
		recursiveWhere = " WHERE " + strings.Join(recursiveConds, " AND ")
	}
	return anchorWhere, recursiveWhere, nil
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
