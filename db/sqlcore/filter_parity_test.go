package sqlcore

// filter_parity_test.go is the guard on AG-5.
//
// filterConds is now a one-line call to maniflex.BuildFilterSQL, so these
// assertions pass trivially today — that is the point. They exist so that the
// day someone "just adds an operator here" and re-forks the renderer, the fork
// fails in CI instead of shipping as a number that disagrees with the list it
// summarises. That is exactly how AG-1, AG-4 and QF-3's aggregate half reached
// production.
//
// The behavioural table for what the SQL should actually say lives in the core
// package's filter_sql_test.go. This file only asserts the two are the same.
//
// Run:
//
//	go test ./db/sqlcore/ -run TestFilterCondsDelegates

import (
	"reflect"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

func parityModel() *maniflex.ModelMeta {
	str := reflect.TypeOf("")
	return &maniflex.ModelMeta{
		Name:      "Ticket",
		TableName: "tickets",
		Fields: []maniflex.FieldMeta{
			{Name: "Title", Type: str, Tags: maniflex.FieldTags{DBName: "title", JSONName: "title"}},
			{Name: "OwnerID", Type: str, Tags: maniflex.FieldTags{DBName: "owner_id", JSONName: "ownerId"}},
			{Name: "Amount", Type: reflect.TypeOf(0), Tags: maniflex.FieldTags{DBName: "amount", JSONName: "amount"}},
			{Name: "Resolved", Type: reflect.TypeOf(false), Tags: maniflex.FieldTags{DBName: "resolved", JSONName: "resolved"}},
			{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}), Tags: maniflex.FieldTags{DBName: "created_at", JSONName: "createdAt"}},
		},
	}
}

// everyOperator is the full set the framework renders. A new operator added to
// FilterOperator without a line here is a gap in this guard, so keep it total.
var everyOperator = []maniflex.FilterOperator{
	maniflex.OpEq, maniflex.OpNeq, maniflex.OpGt, maniflex.OpGte, maniflex.OpLt, maniflex.OpLte,
	maniflex.OpLike, maniflex.OpILike, maniflex.OpContains, maniflex.OpStartsWith, maniflex.OpEndsWith,
	maniflex.OpIn, maniflex.OpNotIn, maniflex.OpIsNull, maniflex.OpNotNull, maniflex.OpBetween,
	maniflex.OpEqField, maniflex.OpNeqField, maniflex.OpGtField,
	maniflex.OpGteField, maniflex.OpLtField, maniflex.OpLteField,
}

func TestFilterCondsDelegates(t *testing.T) {
	t.Parallel()

	values := []struct {
		name string
		val  any
	}{
		{"csv", "a,b"},
		{"single", "a"},
		{"empty", ""},
		{"go_slice", []string{"a", "b"}},
		{"any_slice", []any{"a", "b"}},
		{"number", 42},
		{"bool_word", "false"},
		{"time", time.Date(2026, 8, 3, 10, 30, 0, 0, time.UTC)},
		{"like_meta", "50%"},
		{"between_pair", "5,25"},
	}

	fields := []string{"title", "amount", "resolved", "createdAt", "nope"}

	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		for _, op := range everyOperator {
			for _, v := range values {
				for _, field := range fields {
					name := driverName(driver) + "/" + string(op) + "/" + v.name + "/" + field
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						f := &maniflex.FilterExpr{Field: field, Operator: op, Value: v.val}
						assertSameSQL(t, driver, []*maniflex.FilterExpr{f})
					})
				}
			}
		}
	}
}

func TestFilterCondsDelegates_ShapesBeyondOneFlatFilter(t *testing.T) {
	// Grouping, nil entries, locale and nested columns — the parts of the
	// renderer that are not the operator switch, and where the two
	// implementations had actually drifted.
	t.Parallel()

	cases := []struct {
		name    string
		filters []*maniflex.FilterExpr
	}{
		{"empty", nil},
		{"only_nils", []*maniflex.FilterExpr{nil, nil}},
		{"nil_among_real", []*maniflex.FilterExpr{nil, {Field: "title", Operator: maniflex.OpEq, Value: "a"}, nil}},
		{"two_ungrouped", []*maniflex.FilterExpr{
			{Field: "title", Operator: maniflex.OpEq, Value: "a"},
			{Field: "amount", Operator: maniflex.OpGt, Value: 5},
		}},
		{"one_group", []*maniflex.FilterExpr{
			{Field: "title", Operator: maniflex.OpEq, Value: "a", Group: 1},
			{Field: "title", Operator: maniflex.OpEq, Value: "b", Group: 1},
		}},
		{"field_cmp_same_class", []*maniflex.FilterExpr{
			{Field: "amount", Operator: maniflex.OpGteField, ValueField: "amount"},
		}},
		{"field_cmp_json_spelling", []*maniflex.FilterExpr{
			{Field: "title", Operator: maniflex.OpEqField, ValueField: "ownerId"},
		}},
		{"field_cmp_unknown_rhs_fails_closed", []*maniflex.FilterExpr{
			{Field: "amount", Operator: maniflex.OpGteField, ValueField: "nope"},
		}},
		{"field_cmp_empty_rhs_fails_closed", []*maniflex.FilterExpr{
			{Field: "amount", Operator: maniflex.OpGteField},
		}},
		{"field_cmp_mixed_with_a_literal_filter", []*maniflex.FilterExpr{
			{Field: "amount", Operator: maniflex.OpGteField, ValueField: "amount"},
			{Field: "title", Operator: maniflex.OpEq, Value: "a"},
		}},
		{"groups_declared_out_of_order", []*maniflex.FilterExpr{
			{Field: "title", Operator: maniflex.OpEq, Value: "a", Group: 3},
			{Field: "amount", Operator: maniflex.OpEq, Value: 1, Group: 1},
			{Field: "title", Operator: maniflex.OpEq, Value: "b", Group: 3},
			{Field: "amount", Operator: maniflex.OpEq, Value: 2, Group: 1},
		}},
		{"ungrouped_and_grouped", []*maniflex.FilterExpr{
			{Field: "amount", Operator: maniflex.OpGt, Value: 5},
			{Field: "title", Operator: maniflex.OpEq, Value: "a", Group: 1},
			{Field: "title", Operator: maniflex.OpEq, Value: "b", Group: 1},
		}},
		{"locale", []*maniflex.FilterExpr{
			{Field: "name", Operator: maniflex.OpILike, Value: "x", IsLocale: true, LocaleKey: "ar"},
		}},
		{"nested", []*maniflex.FilterExpr{
			{Operator: maniflex.OpEq, Value: "active", IsNested: true, RelationKey: "author", NestedField: "status"},
		}},
		{"locale_and_nested_and_flat", []*maniflex.FilterExpr{
			{Field: "name", Operator: maniflex.OpEq, Value: "x", IsLocale: true, LocaleKey: "en"},
			{Operator: maniflex.OpEq, Value: "active", IsNested: true, RelationKey: "author", NestedField: "status"},
			{Field: "amount", Operator: maniflex.OpBetween, Value: "1,9"},
		}},
	}

	for _, driver := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		for _, tc := range cases {
			t.Run(driverName(driver)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				assertSameSQL(t, driver, tc.filters)
			})
		}
	}
}

// assertSameSQL runs one filter set through the adapter's renderer and through
// the core builder with a fresh binder each time, and requires both the SQL and
// the bound arguments to match. Arguments matter as much as text: on SQLite a
// placeholder binds by its position in the statement, so two renderers can emit
// identical SQL and still bind different values.
func assertSameSQL(t *testing.T, driver maniflex.DriverType, filters []*maniflex.FilterExpr) {
	t.Helper()
	model := parityModel()

	adapterPH := &ph{driver: driver}
	adapterSQL := filterConds(model, filters, driver, adapterPH)

	corePH := NewPlaceholderBuilder(driver)
	coreSQL := maniflex.BuildFilterSQL(model, filters, driver, corePH)

	if adapterSQL != coreSQL {
		t.Errorf("SQL diverged:\n adapter %s\n core    %s", adapterSQL, coreSQL)
	}
	if !reflect.DeepEqual(adapterPH.args, corePH.Args()) {
		t.Errorf("args diverged:\n adapter %#v\n core    %#v", adapterPH.args, corePH.Args())
	}
}

// driverName labels a subtest. DriverType is an integer, so the obvious
// string(driver) yields one rune rather than the digits.
func driverName(d maniflex.DriverType) string {
	if d == maniflex.Postgres {
		return "postgres"
	}
	return "sqlite"
}
