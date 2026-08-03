package sqlcore

import "github.com/xaleel/maniflex"

// q quotes an SQL identifier (table name, column name, alias) with ANSI
// double-quote delimiters, supported by both SQLite and PostgreSQL.
//
// Quoting is needed for three reasons:
//
//  1. Reserved words: a model named "order" or "group" would produce
//     "CREATE TABLE order" which is a syntax error in every SQL dialect.
//
//  2. Case sensitivity: unquoted identifiers are folded to lowercase by
//     Postgres; quoting preserves the exact name from the struct tag.
//
//  3. Safety-by-construction: even though table/column names come from
//     struct reflection (not user input), quoting removes an entire class
//     of potential breakage if a future code path ever passes user-derived
//     names through the same builder functions.
//
// The only transformation applied is doubling any embedded double-quote
// characters (the ANSI escape for a literal " inside a quoted identifier).
// Dots are NOT passed through q — callers like q(table)+"."+q(col) split
// them deliberately.
//
// The implementation is maniflex.Quote. It used to be a third copy of the same
// four characters, alongside the core package's own; identifier quoting has to
// mean exactly one thing or the shared filter builder and this adapter can
// disagree about what a column is called (audit AG-5).
func q(name string) string {
	return maniflex.Quote(name)
}

// Quote is the exported version of q for packages that need to build SQL
// fragments with consistent identifier quoting (e.g. validate.UniqueField
// composing parameterised SELECT COUNT(*) queries). The encoding is ANSI
// SQL §5.2: double-quote delimiters with embedded quotes doubled. Works
// with both Postgres and SQLite.
func Quote(name string) string {
	return q(name)
}
