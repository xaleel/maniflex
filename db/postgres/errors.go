package postgres

import (
	"strings"

	"github.com/xaleel/maniflex"

	"github.com/lib/pq"
)

// NormalizeError converts a lib/pq constraint error into a *maniflex.ErrConstraint
// so the DB step returns 409 Conflict instead of an opaque 500. Errors that are
// not constraint violations are returned unchanged.
//
// Open/OpenWithConfig register this with the sqlcore adapter via
// SetErrorNormalizer; it satisfies the sqlcore.ErrorNormalizer signature.
// It is exported so callers that build a sqlcore.Adapter directly (e.g. tests)
// can register the same behaviour.
//
// Postgres reports constraint violations as a typed *pq.Error carrying a
// SQLSTATE code: 23505 (unique_violation) and 23503 (foreign_key_violation).
func NormalizeError(err error, table string) error {
	pqErr, ok := err.(*pq.Error)
	if !ok {
		return err
	}
	switch pqErr.Code {
	case "23505": // unique_violation
		cols := extractColumns(pqErr.Detail)
		return &maniflex.ErrConstraint{
			Kind:    maniflex.ConstraintUnique,
			Table:   table,
			Column:  firstOf(cols),
			Columns: cols,
			Detail:  pqErr.Detail,
		}
	case "23502": // not_null_violation — pqErr.Column names the offending column
		return &maniflex.ErrConstraint{
			Kind:    maniflex.ConstraintNotNull,
			Table:   table,
			Column:  pqErr.Column,
			Columns: nonEmpty(pqErr.Column),
			Detail:  pqErr.Message,
		}
	case "23503": // foreign_key_violation
		return &maniflex.ErrConstraint{
			Kind:   maniflex.ConstraintForeignKey,
			Table:  table,
			Detail: pqErr.Detail,
		}
	}
	return err
}

// extractColumns parses every column named by a Postgres error Detail string.
//
//	"Key (email)=(foo@bar.com) already exists."   → ["email"]
//	"Key (email, name)=(foo, bar) already exists." → ["email", "name"]
//
// It used to return the first column only, so a violation of
// UNIQUE(phone_number, owner_id) was reported against phone_number alone —
// blaming one column for a constraint that neither violates by itself
// (audit 11D.1).
func extractColumns(detail string) []string {
	start := strings.Index(detail, "Key (")
	if start < 0 {
		return nil
	}
	inner := detail[start+len("Key ("):]
	end := strings.Index(inner, ")")
	if end < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(inner[:end], ",") {
		if col := strings.TrimSpace(part); col != "" {
			out = append(out, col)
		}
	}
	return out
}

// firstOf returns the first column, or "" — what the pre-Columns
// ErrConstraint.Column field carried, kept so existing consumers read the same
// value they always did.
func firstOf(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	return cols[0]
}

// nonEmpty wraps a single column name as a slice, or nil when it is empty.
func nonEmpty(col string) []string {
	if col == "" {
		return nil
	}
	return []string{col}
}
