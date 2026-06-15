package sqlite

import (
	"strings"

	"maniflex"
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
		return &maniflex.ErrConstraint{
			Table:  table,
			Column: extractColumn(msg),
			Detail: msg,
		}
	}
	if strings.Contains(msg, "FOREIGN KEY constraint failed") {
		return &maniflex.ErrConstraint{
			Table:  table,
			Detail: msg,
		}
	}
	return err
}

// extractColumn parses the column name from a SQLite UNIQUE constraint message.
// "UNIQUE constraint failed: users.email" → "email"
// "UNIQUE constraint failed: users.email, users.name" → "email" (first only)
func extractColumn(msg string) string {
	idx := strings.Index(msg, "failed: ")
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len("failed: "):]
	first := strings.TrimSpace(strings.SplitN(rest, ",", 2)[0])
	// Strip the "table." prefix if present.
	if dot := strings.Index(first, "."); dot >= 0 {
		return first[dot+1:]
	}
	return first
}
