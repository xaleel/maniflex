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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyRaw(c.in); got != c.want {
				t.Errorf("classifyRaw(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
