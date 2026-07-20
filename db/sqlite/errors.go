package sqlite

import (
	"strings"

	"github.com/xaleel/maniflex"
)

// NormalizeError converts a modernc.org/sqlite constraint error into a
// *maniflex.ErrConstraint so the DB step returns 409 Conflict instead of an opaque
// 500. Errors that are not constraint violations are returned unchanged.
//
// Open registers this with the sqlcore adapter via SetErrorNormalizer; it
// satisfies the sqlcore.ErrorNormalizer signature. It is exported so callers
// that build a sqlcore.Adapter directly (e.g. tests) can register the same
// behaviour.
//
// SQLite (modernc) reports constraint violations only in the error message
// text — there is no typed error or stable code — so detection is string-based:
//
//	"UNIQUE constraint failed: users.email"
//	"FOREIGN KEY constraint failed"
func NormalizeError(err error, table string) error {
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") {
		cols := extractColumns(msg)
		return &maniflex.ErrConstraint{
			Kind:    maniflex.ConstraintUnique,
			Table:   table,
			Column:  firstOf(cols),
			Columns: cols,
			Detail:  msg,
		}
	}
	if strings.Contains(msg, "NOT NULL constraint failed") {
		cols := extractColumns(msg)
		return &maniflex.ErrConstraint{
			Kind:    maniflex.ConstraintNotNull,
			Table:   table,
			Column:  firstOf(cols),
			Columns: cols,
			Detail:  msg,
		}
	}
	if strings.Contains(msg, "FOREIGN KEY constraint failed") {
		return &maniflex.ErrConstraint{
			Kind:   maniflex.ConstraintForeignKey,
			Table:  table,
			Detail: msg,
		}
	}
	return err
}

// extractColumns parses every column named by a SQLite constraint message.
//
//	"UNIQUE constraint failed: users.email"                → ["email"]
//	"UNIQUE constraint failed: users.email, users.name"    → ["email", "name"]
//	"NOT NULL constraint failed: users.note (1299)"        → ["note"]
//
// It used to return the first column only, so a violation of
// UNIQUE(phone_number, owner_id) was reported against phone_number alone —
// blaming one column for a constraint that neither violates by itself (audit
// 11D.1). It also handed through the extended result code the driver appends to
// its message, which reached clients as a field named "note (1299)".
func extractColumns(msg string) []string {
	// LastIndex, not Index: the driver wraps its own text, so the message
	// arrives as "constraint failed: UNIQUE constraint failed: t.a, t.b" and the
	// first match leaves "UNIQUE constraint failed: t.a" as the first column.
	idx := strings.LastIndex(msg, "failed: ")
	if idx < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(msg[idx+len("failed: "):], ",") {
		col := strings.TrimSpace(part)
		// Everything from the first space on is the driver's own trailer, not
		// part of the name — a column name cannot contain one unquoted.
		if sp := strings.IndexAny(col, " \t"); sp >= 0 {
			col = col[:sp]
		}
		// Strip the "table." prefix if present.
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			col = col[dot+1:]
		}
		if col != "" {
			out = append(out, col)
		}
	}
	return out
}

// firstOf returns the first column, or "" — what the pre-Columns ErrConstraint.Column
// field carried, kept so existing consumers read the same value they always did.
func firstOf(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	return cols[0]
}
