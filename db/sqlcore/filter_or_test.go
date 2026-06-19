package sqlcore

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// filterCondsSQL is a test helper that runs filterConds and returns the SQL
// string plus the collected args, without needing a real *sql.DB.
func filterCondsSQL(driver maniflex.DriverType, model *maniflex.ModelMeta, filters []*maniflex.FilterExpr) (string, []any) {
	p := &ph{driver: driver}
	sql := filterConds(model, filters, driver, p)
	return sql, p.args
}

func postModel() *maniflex.ModelMeta {
	return &maniflex.ModelMeta{
		Name:      "Post",
		TableName: "posts",
	}
}

func f(field string, op maniflex.FilterOperator, val any, group int) *maniflex.FilterExpr {
	return &maniflex.FilterExpr{Field: field, Operator: op, Value: val, Group: group}
}

// ── SQLite (? placeholders) ───────────────────────────────────────────────────

func TestFilterConds_SingleUngrouped_SQLite(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", -1),
	})
	if sql != `"posts"."status" = ?` {
		t.Fatalf("unexpected sql: %q", sql)
	}
	if len(args) != 1 || args[0] != "draft" {
		t.Fatalf("unexpected args: %v", args)
	}
}

// Hand-built filters left at the FilterExpr zero value (Group == 0) must AND,
// not silently OR. Regression for the ownership-scoping leak where a scoped
// list (e.g. user_id=A AND archived=false) matched far more rows than intended.
func TestFilterConds_ZeroValueGroupAnds_SQLite(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		{Field: "user_id", Operator: maniflex.OpEq, Value: "A"},   // Group 0 (zero value)
		{Field: "archived", Operator: maniflex.OpEq, Value: false}, // Group 0 (zero value)
	})
	want := `"posts"."user_id" = ? AND "posts"."archived" = ?`
	if sql != want {
		t.Fatalf("zero-value Group must AND, not OR\n got  %q\n want %q", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 args, got %v", args)
	}
}

func TestFilterConds_ORGroup_SQLite(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
	})
	want := `("posts"."status" = ? OR "posts"."status" = ?)`
	if sql != want {
		t.Fatalf("got  %q\nwant %q", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 args, got %v", args)
	}
}

func TestFilterConds_ORGroupAndUngrouped_SQLite(t *testing.T) {
	// (A OR B) AND C
	filters := []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
		f("title", maniflex.OpEq, "Hello", -1),
	}
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), filters)
	if !strings.Contains(sql, " OR ") {
		t.Fatalf("expected OR in sql: %q", sql)
	}
	if !strings.Contains(sql, " AND ") {
		t.Fatalf("expected AND in sql: %q", sql)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args, got %v", args)
	}
}

func TestFilterConds_TwoSeparateORGroups_SQLite(t *testing.T) {
	// group 1: (A OR B)  AND  group 2: (C OR D)
	filters := []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
		f("title", maniflex.OpEq, "Alpha", 2),
		f("title", maniflex.OpEq, "Beta", 2),
	}
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), filters)
	// Should look like  ("posts"."status" = ? OR ...) AND ("posts"."title" = ? OR ...)
	orCount := strings.Count(sql, " OR ")
	andCount := strings.Count(sql, " AND ")
	if orCount != 2 {
		t.Fatalf("expected 2 OR, got %d in: %q", orCount, sql)
	}
	if andCount != 1 {
		t.Fatalf("expected 1 AND, got %d in: %q", andCount, sql)
	}
	if len(args) != 4 {
		t.Fatalf("want 4 args, got %v", args)
	}
}

func TestFilterConds_GroupOrderDeterministic(t *testing.T) {
	// groups added in reverse order — SQL should still put lower group first
	filters := []*maniflex.FilterExpr{
		f("title", maniflex.OpEq, "Z", 2),
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
		f("title", maniflex.OpEq, "A", 2),
	}
	sql1, _ := filterCondsSQL(maniflex.SQLite, postModel(), filters)

	filters2 := []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
		f("title", maniflex.OpEq, "Z", 2),
		f("title", maniflex.OpEq, "A", 2),
	}
	sql2, _ := filterCondsSQL(maniflex.SQLite, postModel(), filters2)

	// group 1 (status) must appear before group 2 (title) in both
	if !strings.Contains(sql1, "status") || !strings.Contains(sql2, "status") {
		t.Fatalf("status missing: %q / %q", sql1, sql2)
	}
	idx0_1 := strings.Index(sql1, "status")
	idx2_1 := strings.Index(sql1, "title")
	if idx0_1 > idx2_1 {
		t.Fatalf("group 0 should precede group 2 in: %q", sql1)
	}
}

func TestFilterConds_Between_SQLite(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("views", maniflex.OpBetween, "100,500", -1),
	})
	want := `("posts"."views" >= ? AND "posts"."views" <= ?)`
	if sql != want {
		t.Fatalf("got  %q\nwant %q", sql, want)
	}
	if len(args) != 2 || args[0] != "100" || args[1] != "500" {
		t.Fatalf("unexpected args: %v", args)
	}
}

// A between inside an OR group must stay self-contained so OR precedence does
// not leak across its two bounds.
func TestFilterConds_BetweenInORGroup_SQLite(t *testing.T) {
	sql, _ := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("views", maniflex.OpBetween, "100,500", 1),
		f("status", maniflex.OpEq, "draft", 1),
	})
	want := `(("posts"."views" >= ? AND "posts"."views" <= ?) OR "posts"."status" = ?)`
	if sql != want {
		t.Fatalf("got  %q\nwant %q", sql, want)
	}
}

// ── Postgres ($N placeholders) ────────────────────────────────────────────────

func TestFilterConds_ORGroup_Postgres(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.Postgres, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpEq, "draft", 1),
		f("status", maniflex.OpEq, "published", 1),
	})
	want := `("posts"."status" = $1 OR "posts"."status" = $2)`
	if sql != want {
		t.Fatalf("got  %q\nwant %q", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 args, got %v", args)
	}
}
