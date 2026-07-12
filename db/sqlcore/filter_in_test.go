package sqlcore

// An in/not_in filter carrying no values must not produce "col IN ()".
//
// The HTTP path rejects it at parse time, but a FilterExpr built by hand in Go
// (typed List/QueryParams, a tenancy middleware) reaches the adapter unparsed. It
// only ever surfaced as a 500 on Postgres — SQLite tolerates "IN ()" as a dialect
// extension and quietly evaluates it to false — so assert on the SQL itself
// rather than on a query result (BUG-7).

import (
	"testing"

	"github.com/xaleel/maniflex"
)

func TestFilterConds_EmptyIn_MatchesNothing(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		sql, args := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
			f("status", maniflex.OpIn, "", -1),
		})
		if sql != "1=0" {
			t.Errorf("%v: empty IN produced %q, want 1=0", driver, sql)
		}
		if len(args) != 0 {
			t.Errorf("%v: empty IN bound %d args, want 0", driver, len(args))
		}
	}
}

func TestFilterConds_EmptyNotIn_MatchesEverything(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		sql, _ := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
			f("status", maniflex.OpNotIn, "", -1),
		})
		if sql != "1=1" {
			t.Errorf("%v: empty NOT IN produced %q, want 1=1", driver, sql)
		}
	}
}

// A value whose entries all drop out (separators only) is equally empty.
func TestFilterConds_SeparatorsOnlyIn_MatchesNothing(t *testing.T) {
	sql, _ := filterCondsSQL(maniflex.Postgres, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpIn, ",, ,", -1),
	})
	if sql != "1=0" {
		t.Errorf("separators-only IN produced %q, want 1=0", sql)
	}
}

// The non-empty path is untouched.
func TestFilterConds_InWithValues_Unchanged(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.Postgres, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpIn, "draft,archived", -1),
	})
	if sql != `"posts"."status" IN ($1, $2)` {
		t.Errorf("unexpected sql: %q", sql)
	}
	if len(args) != 2 || args[0] != "draft" || args[1] != "archived" {
		t.Errorf("unexpected args: %v", args)
	}
}
