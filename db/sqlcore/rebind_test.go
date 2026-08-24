package sqlcore

import (
	"testing"

	"github.com/xaleel/maniflex"
)

func TestRebind(t *testing.T) {
	cases := []struct {
		name   string
		driver maniflex.DriverType
		in     string
		want   string
	}{
		{"sqlite passthrough", maniflex.SQLite, "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = ? AND b = ?"},
		{"postgres two params", maniflex.Postgres, "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"postgres no params", maniflex.Postgres, "SELECT 1", "SELECT 1"},
		{"postgres qmark inside literal untouched", maniflex.Postgres, "SELECT * FROM t WHERE label = 'a?b' AND a = ?", "SELECT * FROM t WHERE label = 'a?b' AND a = $1"},
		{"postgres escaped quote in literal", maniflex.Postgres, "UPDATE t SET s = 'it''s a ? test' WHERE a = ?", "UPDATE t SET s = 'it''s a ? test' WHERE a = $1"},

		// Everything below tracked only single-quote state before, so a ? in any
		// other non-code region became a placeholder and shifted the numbering of
		// every real one after it (audit O7).
		{
			"qmark inside a quoted identifier",
			maniflex.Postgres,
			`SELECT "we?rd" FROM t WHERE b = ?`,
			`SELECT "we?rd" FROM t WHERE b = $1`,
		},
		{
			"doubled quote inside a quoted identifier",
			maniflex.Postgres,
			`SELECT "we""?rd" FROM t WHERE b = ?`,
			`SELECT "we""?rd" FROM t WHERE b = $1`,
		},
		{
			"qmark in a line comment before the placeholder",
			maniflex.Postgres,
			"-- really?\nSELECT * FROM t WHERE a = ?",
			"-- really?\nSELECT * FROM t WHERE a = $1",
		},
		{
			"qmark in a block comment",
			maniflex.Postgres,
			"SELECT /* what? */ * FROM t WHERE a = ?",
			"SELECT /* what? */ * FROM t WHERE a = $1",
		},
		{
			// The ? sits in the OUTER comment, after the inner one closes. Closing
			// at the first */ would leave it in code, so this distinguishes nested
			// handling from flat; a ? inside the inner comment would not.
			"nested block comment",
			maniflex.Postgres,
			"SELECT /* a /* b */ c ? */ * FROM t WHERE a = ?",
			"SELECT /* a /* b */ c ? */ * FROM t WHERE a = $1",
		},
		{
			"dollar-quoted body",
			maniflex.Postgres,
			"INSERT INTO t VALUES ($$a ? b$$, ?)",
			"INSERT INTO t VALUES ($$a ? b$$, $1)",
		},
		{
			"dollar-quoted body with a tag",
			maniflex.Postgres,
			"INSERT INTO t VALUES ($tag$a ? b$tag$, ?)",
			"INSERT INTO t VALUES ($tag$a ? b$tag$, $1)",
		},
		{
			// A backslash escapes the quote only in an E-string, so the literal
			// runs on. Reading it as closed left the ? inside it rewritten and the
			// real one after it untouched — wrong at both ends.
			"escaped quote inside an E-string",
			maniflex.Postgres,
			`SELECT * FROM t WHERE a = E'\'?' AND b = ?`,
			`SELECT * FROM t WHERE a = E'\'?' AND b = $1`,
		},
		{
			// ?| and ?& are jsonb operators and can never be placeholders, so they
			// are safe to preserve. A bare ? stays ambiguous and is still rewritten.
			"jsonb any-key operator survives",
			maniflex.Postgres,
			`SELECT * FROM t WHERE data ?| array['a','b'] AND id = ?`,
			`SELECT * FROM t WHERE data ?| array['a','b'] AND id = $1`,
		},
		{
			"jsonb all-keys operator survives",
			maniflex.Postgres,
			`SELECT * FROM t WHERE data ?& array['a'] AND id = ?`,
			`SELECT * FROM t WHERE data ?& array['a'] AND id = $1`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rebind(c.driver, c.in); got != c.want {
				t.Errorf("rebind(%v, %q) = %q, want %q", c.driver, c.in, got, c.want)
			}
		})
	}
}

func TestClassifyRaw(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want rawKind
	}{
		{"select", "SELECT * FROM t", rawSelect},
		{"select lowercased with leading space", "  select 1", rawSelect},
		{"cte select", "WITH x AS (SELECT 1) SELECT * FROM x", rawSelect},
		{"plain update", "UPDATE t SET a = 1 WHERE id = ?", rawExec},
		{"insert", "INSERT INTO t (a) VALUES (?)", rawExec},
		{"insert returning", "INSERT INTO t (a) VALUES (?) RETURNING id", rawReturning},
		{"update returning", "UPDATE t SET a = 1 WHERE id = ? RETURNING id, a", rawReturning},
		{"delete returning", "DELETE FROM t WHERE id = ? RETURNING id", rawReturning},
		{"returning word inside literal is not a clause", "UPDATE t SET note = 'returning soon' WHERE id = ?", rawExec},
		{"returning inside line comment ignored", "UPDATE t SET a = 1 WHERE id = ? -- returning\n", rawExec},

		// classifyRaw shares rebind's scanner, so the regions rebind learned to
		// respect are regions keyword detection stops reading. Before that, a
		// quoted identifier and a dollar-quoted body were plain code here, and the
		// word inside one was taken for the clause — routing a plain exec onto the
		// query path, where it waits for a result set that never arrives.
		{"returning inside a quoted identifier is not a clause", `UPDATE t SET "returning" = 1 WHERE id = ?`, rawExec},
		{"returning inside a dollar-quoted body is not a clause", `DO $$ BEGIN PERFORM 'returning'; END $$`, rawExec},
		{"returning inside a block comment is not a clause", "UPDATE t SET a = 1 /* returning */ WHERE id = ?", rawExec},
		{"a real returning after a quoted identifier still counts", `UPDATE t SET "returning" = 1 WHERE id = ? RETURNING id`, rawReturning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyRaw(c.in); got != c.want {
				t.Errorf("classifyRaw(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
