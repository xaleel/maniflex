package maniflex

// AG-1/AG-4 — the aggregate WHERE builder must render the same filter the list
// path renders. It is a separate implementation from db/sqlcore's filterConds,
// and every capability one grew that the other did not became a silently wrong
// number on an endpoint whose whole job is reporting numbers.

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func aggModel() *ModelMeta {
	str := reflect.TypeOf("")
	return &ModelMeta{
		Name:      "Ticket",
		TableName: "tickets",
		Fields: []FieldMeta{
			{Name: "Title", Type: str, Tags: FieldTags{DBName: "title", JSONName: "title"}},
			{Name: "OwnerID", Type: str, Tags: FieldTags{DBName: "owner_id", JSONName: "ownerId"}},
			{Name: "Amount", Type: reflect.TypeOf(0),
				Tags: FieldTags{DBName: "amount", JSONName: "amount"}},
			{Name: "Resolved", Type: reflect.TypeOf(false),
				Tags: FieldTags{DBName: "resolved", JSONName: "resolved"}},
			{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}),
				Tags: FieldTags{DBName: "created_at", JSONName: "createdAt"}},
		},
	}
}

func aggWhereSQL(t *testing.T, driver DriverType, filters []*FilterExpr) (string, []any) {
	t.Helper()
	pb := newAggPH(driver)
	sql, err := aggBuildWhere(aggModel(), filters, driver, pb)
	if err != nil {
		t.Fatalf("aggBuildWhere: %v", err)
	}
	return strings.TrimPrefix(sql, " WHERE "), pb.args
}

func aggF(field string, op FilterOperator, val any, group int) *FilterExpr {
	return &FilterExpr{Field: field, Operator: op, Value: val, Group: group}
}

// AG-1. The list path ORs filters sharing a Group; the aggregate path ANDed
// every filter regardless, so a grouped-OR count collapsed to the AND
// intersection — usually zero — with no error.
func TestAggBuildWhere_ORGroup(t *testing.T) {
	sql, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("title", OpEq, "a", 1),
		aggF("title", OpEq, "b", 1),
	})
	want := `("tickets"."title" = ? OR "tickets"."title" = ?)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 args, got %v", args)
	}
}

// Ungrouped filters keep ANDing — a zero-value Group must never be read as a
// request to OR, which would widen a forced filter.
func TestAggBuildWhere_UngroupedAnds(t *testing.T) {
	sql, _ := aggWhereSQL(t, SQLite, []*FilterExpr{
		{Field: "owner_id", Operator: OpEq, Value: "u1"}, // Group 0 (zero value)
		{Field: "resolved", Operator: OpEq, Value: true}, // Group 0 (zero value)
	})
	want := `"tickets"."owner_id" = ? AND "tickets"."resolved" = ?`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

func TestAggBuildWhere_GroupedAndUngroupedCombine(t *testing.T) {
	sql, _ := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("owner_id", OpEq, "u1", -1),
		aggF("title", OpEq, "a", 1),
		aggF("title", OpEq, "b", 1),
	})
	want := `"tickets"."owner_id" = ? AND ("tickets"."title" = ? OR "tickets"."title" = ?)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

// AG-4. between had no case, so sqlOp's fallthrough rendered it as "=" against
// the raw "lo,hi" string — a predicate that matches nothing, from a filter the
// endpoint's own allowlist believed it had excluded.
func TestAggBuildWhere_Between(t *testing.T) {
	sql, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("amount", OpBetween, "5,25", -1),
	})
	want := `("tickets"."amount" >= ? AND "tickets"."amount" <= ?)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	if len(args) != 2 || args[0] != "5" || args[1] != "25" {
		t.Fatalf("want the two bounds bound separately, got %v", args)
	}
}

// A between whose value is not exactly two bounds cannot be rendered, and must
// match nothing rather than everything — the same call the list path makes.
func TestAggBuildWhere_MalformedBetweenFailsClosed(t *testing.T) {
	sql, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("amount", OpBetween, "5", -1),
	})
	if sql != "1=0" {
		t.Fatalf("\n got  %q\n want %q", sql, "1=0")
	}
	if len(args) != 0 {
		t.Fatalf("a false predicate binds nothing, got %v", args)
	}
}

// An operator this builder cannot render must fail closed rather than degrade
// to "=", which is what turned AG-4 into a wrong answer instead of an error.
func TestAggBuildWhere_UnknownOperatorFailsClosed(t *testing.T) {
	sql, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("title", FilterOperator("equals"), "a", -1),
	})
	if sql != "1=0" {
		t.Fatalf("\n got  %q\n want %q", sql, "1=0")
	}
	if len(args) != 0 {
		t.Fatalf("a false predicate binds nothing, got %v", args)
	}
}

// QF-3's aggregate half: the value normalisation added to the list path has to
// apply here too, or the same filter counts differently than it lists.
func TestAggBuildWhere_BoolValueCoerced(t *testing.T) {
	_, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("resolved", OpEq, "false", -1),
	})
	if len(args) != 1 || args[0] != false {
		t.Fatalf("bound %#v, want the bool false", args)
	}
}

func TestAggBuildWhere_TimeValueCanonicalised(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 30, 45, 456_000_000, time.UTC)
	_, args := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("created_at", OpLte, when, -1),
	})
	if len(args) != 1 || args[0] != CanonicalTime(when) {
		t.Fatalf("bound %#v, want %q", args, CanonicalTime(when))
	}
}

func TestAggBuildWhere_JSONFieldNameResolves(t *testing.T) {
	sql, _ := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("ownerId", OpEq, "u1", -1),
	})
	want := `"tickets"."owner_id" = ?`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

func TestAggBuildWhere_UnknownFieldFailsClosed(t *testing.T) {
	sql, _ := aggWhereSQL(t, SQLite, []*FilterExpr{
		aggF("nope", OpEq, "x", -1),
	})
	if sql != "1=0" {
		t.Fatalf("\n got  %q\n want %q", sql, "1=0")
	}
}

// Postgres numbers its placeholders, and an OR group must not disturb the
// ordering.
func TestAggBuildWhere_ORGroup_Postgres(t *testing.T) {
	sql, _ := aggWhereSQL(t, Postgres, []*FilterExpr{
		aggF("owner_id", OpEq, "u1", -1),
		aggF("title", OpEq, "a", 1),
		aggF("title", OpEq, "b", 1),
	})
	want := `"tickets"."owner_id" = $1 AND ("tickets"."title" = $2 OR "tickets"."title" = $3)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

// Group ordering must be deterministic, or the same query produces different
// SQL between runs and defeats statement caching.
func TestAggBuildWhere_GroupOrderDeterministic(t *testing.T) {
	filters := []*FilterExpr{
		aggF("title", OpEq, "b", 2),
		aggF("owner_id", OpEq, "u1", 1),
		aggF("title", OpEq, "a", 2),
		aggF("owner_id", OpEq, "u2", 1),
	}
	first, _ := aggWhereSQL(t, SQLite, filters)
	for i := 0; i < 5; i++ {
		again, _ := aggWhereSQL(t, SQLite, filters)
		if again != first {
			t.Fatalf("group order is not deterministic:\n %q\n %q", first, again)
		}
	}
}
