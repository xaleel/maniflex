package sqlcore

import (
	"fmt"

	"github.com/xaleel/maniflex"
)

// cursor.go — keyset (cursor) pagination SQL for FindMany (roadmap 4.8).
//
// When QueryParams.Cursor is set, the data query walks the dataset ordered by
// (cursor field, id) with a boundary predicate instead of LIMIT/OFFSET, so it
// neither skips nor duplicates rows when the table is written between fetches.
// The COUNT is skipped — keyset pagination intentionally avoids the O(table)
// count and reports has_more (derived from an over-fetch of one row) instead.

// cursorWhereCond returns the keyset boundary predicate, or "" on the first page
// (no token yet). It expands the row-value comparison
// (cursor_field, id) > (afterValue, afterID) into the portable two-clause form
// so it behaves identically on Postgres and SQLite. A descending walk flips > to <.
func cursorWhereCond(model *maniflex.ModelMeta, cur *maniflex.CursorParams, p *ph) string {
	if !cur.HasToken {
		return ""
	}
	col := q(model.TableName) + "." + q(cur.Field)
	idcol := q(model.TableName) + "." + q("id")
	cmp := ">"
	if cur.Direction == maniflex.SortDesc {
		cmp = "<"
	}
	return fmt.Sprintf("(%s %s %s OR (%s = %s AND %s %s %s))",
		col, cmp, p.add(cur.AfterValue),
		col, p.add(cur.AfterValue),
		idcol, cmp, p.add(cur.AfterID),
	)
}

// cursorOrderSQL returns the ORDER BY for a cursor query: the cursor field, then
// id as the tiebreaker, both in the walk direction so the ordering and the
// boundary predicate stay consistent.
func cursorOrderSQL(model *maniflex.ModelMeta, cur *maniflex.CursorParams) string {
	dir := "ASC"
	if cur.Direction == maniflex.SortDesc {
		dir = "DESC"
	}
	col := q(model.TableName) + "." + q(cur.Field)
	idcol := q(model.TableName) + "." + q("id")
	return fmt.Sprintf(" ORDER BY %s %s, %s %s", col, dir, idcol, dir)
}

// cursorDataClauses appends the keyset boundary to conds and returns the ORDER BY
// and LIMIT clauses for a cursor query. The LIMIT over-fetches one row so the
// caller can detect (and trim) a following page. p must already hold the WHERE
// args so placeholder numbering stays in order.
func cursorDataClauses(model *maniflex.ModelMeta, cur *maniflex.CursorParams, limit int, conds []string, p *ph) (outConds []string, orderSQL, limitSQL string) {
	if c := cursorWhereCond(model, cur, p); c != "" {
		conds = append(conds, c)
	}
	return conds, cursorOrderSQL(model, cur), " LIMIT " + p.add(limit+1)
}

// finalizeCursorPage trims an over-fetched cursor page (limit+1 rows) down to
// limit, recording on cur whether more rows follow and, if so, the next-page
// token built from the last kept row. rowKey returns the cursor field value and
// id for the row at index i. It returns the number of rows to keep.
func finalizeCursorPage(cur *maniflex.CursorParams, n, limit int, rowKey func(i int) (any, string)) int {
	if n <= limit {
		return n
	}
	cur.HasMore = true
	v, id := rowKey(limit - 1)
	cur.NextCursor = maniflex.EncodeCursor(v, id)
	return limit
}
