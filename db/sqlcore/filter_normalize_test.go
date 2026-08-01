package sqlcore

// QF-2/QF-3 — filterConds resolves each filter against the model and normalises
// its value, so a FilterExpr built in Go binds the same way one parsed from a
// URL does. Before this, only the URL path normalised, and only for time.

import (
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// A real time.Time in a Go-built filter used to bind as the driver's own
// rendering of the instant, which on SQLite is a different string than the
// CanonicalTimeLayout one the write path stored — so the comparison ran against
// a value that is not in the column.
func TestFilterConds_TimeValueCanonicalised(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 30, 45, 456_000_000, time.UTC)
	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		_, args := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
			f("created_at", maniflex.OpLte, when, -1),
		})
		if len(args) != 1 {
			t.Fatalf("%v: want 1 arg, got %v", driver, args)
		}
		if args[0] != maniflex.CanonicalTime(when) {
			t.Fatalf("%v: bound %#v, want the canonical string %q",
				driver, args[0], maniflex.CanonicalTime(when))
		}
	}
}

// QF-3, at the SQL layer. A bool column is INTEGER on SQLite, so a bound
// "false" stays TEXT and can never compare equal; it must reach the driver as a
// real bool.
func TestFilterConds_BoolWordCoerced(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		_, args := filterCondsSQL(driver, postModel(), []*maniflex.FilterExpr{
			f("archived", maniflex.OpEq, "false", -1),
		})
		if len(args) != 1 {
			t.Fatalf("%v: want 1 arg, got %v", driver, args)
		}
		if args[0] != false {
			t.Fatalf("%v: bound %#v (%T), want the bool false", driver, args[0], args[0])
		}
	}
}

// The json spelling is the one a caller building a filter in Go naturally
// reaches for, since it is the name the model publishes. It must resolve to the
// column rather than becoming a column no table has.
func TestFilterConds_JSONFieldNameResolvesToColumn(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("authorId", maniflex.OpEq, "u1", -1),
	})
	want := `"posts"."author_id" = ?`
	if sql != want {
		t.Fatalf("json field name\n got  %q\n want %q", sql, want)
	}
	if len(args) != 1 || args[0] != "u1" {
		t.Fatalf("unexpected args: %v", args)
	}
}

// A field the model does not have cannot be turned into a predicate. It fails
// closed for the reason buildCond already gives for an unknown operator:
// matching everything deletes the filter, and for a forced filter that means a
// tenant scope silently stops scoping.
func TestFilterConds_UnknownFieldFailsClosed(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("no_such_column", maniflex.OpEq, "x", -1),
	})
	if sql != "1=0" {
		t.Fatalf("unknown field must fail closed\n got  %q\n want %q", sql, "1=0")
	}
	if len(args) != 0 {
		t.Fatalf("a false predicate binds nothing, got %v", args)
	}
}

// An unknown field inside an OR group must not widen the group: 1=0 OR x
// still lets x through, which is correct, but the unknown half must contribute
// nothing rather than everything.
func TestFilterConds_UnknownFieldInORGroupFailsClosed(t *testing.T) {
	sql, _ := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("no_such_column", maniflex.OpEq, "x", 1),
		f("status", maniflex.OpEq, "draft", 1),
	})
	want := `(1=0 OR "posts"."status" = ?)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

// Nested (relation) and locale filters name a column on another table or a key
// inside a JSON document, so neither resolves against the base model's fields.
func TestFilterConds_NestedFilterNotResolvedAgainstBaseModel(t *testing.T) {
	sql, _ := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		{Field: "author_id", Operator: maniflex.OpEq, Value: "u1", Group: -1,
			IsNested: true, RelationKey: "author", NestedField: "email"},
	})
	want := `"author"."email" = ?`
	if sql != want {
		t.Fatalf("nested filter must keep its own column\n got  %q\n want %q", sql, want)
	}
}

// Values on a plain text column are untouched — normalisation only fires for a
// column kind that needs it.
func TestFilterConds_StringValueUntouched(t *testing.T) {
	_, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", -1),
	})
	if len(args) != 1 || args[0] != "draft" {
		t.Fatalf("unexpected args: %v", args)
	}
}
