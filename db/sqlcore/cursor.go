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
//
// A boundary value EncodeCursor cannot represent is an error rather than a page.
// It used to set HasMore first and then assign the empty string EncodeCursor
// returns on failure, and next_cursor is rendered with omitempty — so the client
// got {"has_more": true} and no token, which is a walk it cannot continue and
// cannot detect the end of, with nothing logged (audit O3).
//
// Reporting no further rows instead would be worse: the client would stop,
// believing it had everything, and every row past this boundary would be missing
// from a result that looked complete. Neither silent answer is available, so the
// page fails and says why.
//
// cur is left untouched on that path, so nothing downstream sees the half-set
// state this exists to prevent.
func finalizeCursorPage(cur *maniflex.CursorParams, n, limit int, rowKey func(i int) (any, string)) (int, error) {
	if n <= limit {
		return n, nil
	}
	v, id := rowKey(limit - 1)
	token := maniflex.EncodeCursor(v, id)
	if token == "" {
		return 0, fmt.Errorf(
			"maniflex: cannot encode a next-page cursor for cursor_field %q at row id %q: "+
				"the boundary value (%T) has no cursor encoding. Registration accepts a narrower "+
				"set of column types than this — a NaN or infinite float, or a NULL in a column "+
				"the schema was not migrated to declare NOT NULL, reaches here and cannot be put "+
				"in a token",
			cur.Field, id, v)
	}
	cur.HasMore = true
	cur.NextCursor = token
	return limit, nil
}
