package postgres

import (
	"strings"

	"maniflex"

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
		return &maniflex.ErrConstraint{
			Table:  table,
			Column: extractColumn(pqErr.Detail),
			Detail: pqErr.Detail,
		}
	case "23503": // foreign_key_violation
		return &maniflex.ErrConstraint{
			Table:  table,
			Detail: pqErr.Detail,
		}
	}
	return err
}

// extractColumn parses the column name from a Postgres error Detail string.
// "Key (email)=(foo@bar.com) already exists." → "email"
// "Key (email, name)=(foo, bar) already exists." → "email" (first only)
func extractColumn(detail string) string {
	start := strings.Index(detail, "Key (")
	if start < 0 {
		return ""
	}
	inner := detail[start+len("Key ("):]
	end := strings.Index(inner, ")")
	if end < 0 {
		return ""
	}
	cols := inner[:end]
	return strings.TrimSpace(strings.SplitN(cols, ",", 2)[0])
}
