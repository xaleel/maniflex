package sqlcore

// An operator the query builder does not implement must produce a predicate that
// matches nothing, not one that matches everything.
//
// buildCond's switch fell through to "1=1", which does not narrow a query — it
// deletes the condition. For a client's filter that is a list returning the whole
// table. For a forced filter it is worse: db.Tenancy's scope with a misspelt
// operator is not a narrower scope but no scope, on reads and — since the write
// path reads the row back through those same filters — on writes too. Nothing
// logged either way, and the extra rows look exactly like data.
//
// Only a hand-built FilterExpr can get here: ParseFilterParam checks the operator
// for everything arriving over HTTP. But Operator is a bare string type, so
// Operator: "equals" (the constant OpEq is "eq") compiles, and a forced filter is
// precisely the kind that is built in Go rather than parsed.
//
// maniflex's DB step now rejects such a filter outright with a real error. These
// cover the backstop underneath it, for the paths that do not run that step (the
// typed API, a background context) and for a filter built after it ran.

import (
	"testing"

	"github.com/xaleel/maniflex"
)

func TestFilterConds_UnknownOperator_MatchesNothing(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		sql, args := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
			f("org_id", "equals", "tenant-a", -1), // typo: the constant is "eq"
		})
		if sql != "1=0" {
			t.Errorf("%v: unknown operator produced %q, want 1=0 — a scope the builder "+
				"cannot render must match nothing, not everything", driver, sql)
		}
		if len(args) != 0 {
			t.Errorf("%v: unknown operator bound %d args, want 0", driver, len(args))
		}
	}
}

// The empty operator is the zero value of FilterExpr.Operator, so it is what a
// struct literal that forgets the field carries.
func TestFilterConds_EmptyOperator_MatchesNothing(t *testing.T) {
	sql, _ := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("org_id", "", "tenant-a", -1),
	})
	if sql != "1=0" {
		t.Errorf("empty operator produced %q, want 1=0", sql)
	}
}

// A between needs two bounds. ParseFilterParam checks that; a hand-built one
// reaches the adapter unchecked, and "everything matches" is the one reading of a
// half-written range that cannot be what its author meant.
func TestFilterConds_MalformedBetween_MatchesNothing(t *testing.T) {
	for _, val := range []any{"100", "", "1,2,3"} {
		sql, _ := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
			f("views", maniflex.OpBetween, val, -1),
		})
		if sql != "1=0" {
			t.Errorf("between %q produced %q, want 1=0", val, sql)
		}
	}
}

// The guard must not swallow the operators that do work — a fail-closed default
// is only correct if nothing legitimate lands on it.
func TestFilterConds_EveryValidOperatorRenders(t *testing.T) {
	vals := map[maniflex.FilterOperator]any{
		maniflex.OpIn: "a,b", maniflex.OpNotIn: "a,b", maniflex.OpBetween: "1,2",
	}
	for _, op := range []maniflex.FilterOperator{
		maniflex.OpEq, maniflex.OpNeq, maniflex.OpGt, maniflex.OpGte, maniflex.OpLt,
		maniflex.OpLte, maniflex.OpLike, maniflex.OpILike, maniflex.OpIn, maniflex.OpNotIn,
		maniflex.OpIsNull, maniflex.OpNotNull, maniflex.OpBetween, maniflex.OpContains,
		maniflex.OpStartsWith, maniflex.OpEndsWith,
	} {
		if !op.Valid() {
			t.Errorf("%q: Valid() = false, but it is a shipped operator", op)
		}
		val, ok := vals[op]
		if !ok {
			val = "x"
		}
		for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
			sql, _ := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
				f("status", op, val, -1),
			})
			if sql == "1=0" {
				t.Errorf("%v: operator %q rendered as 1=0 — the fail-closed default swallowed "+
					"a real operator, so every query using it now matches nothing", driver, op)
			}
		}
	}
}

// Valid must reject what the builder cannot render, or the DB step's check and
// the builder's default disagree about which filters are real.
func TestFilterOperator_ValidRejectsUnknown(t *testing.T) {
	for _, op := range []maniflex.FilterOperator{"", "equals", "EQ", "eq ", "exists", "1=1"} {
		if op.Valid() {
			t.Errorf("Valid(%q) = true, want false", op)
		}
	}
}
