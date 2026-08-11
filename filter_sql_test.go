package maniflex

// filter_sql_test.go pins BuildFilterSQL, the one WHERE builder (audit AG-5).
//
// There used to be two — maniflex's aggBuildWhere and db/sqlcore's filterConds
// — kept in agreement by hand, and every capability one grew that the other did
// not shipped as a silently wrong answer: AG-1 (grouped OR), AG-4 (between),
// QF-3's aggregate half (bool coercion). Three P0s from one duplicate.
//
// This is the behavioural table for the merged builder. db/sqlcore's
// filter_parity_test.go guards the delegation itself.
//
// Run this group:
//
//	go test . -run TestBuildFilterSQL

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// filterModel carries one of every field shape the builder resolves against:
// a plain string, a json name that differs from the column, a number, a bool,
// and a time.
func filterModel() *ModelMeta {
	str := reflect.TypeOf("")
	return &ModelMeta{
		Name:      "Ticket",
		TableName: "tickets",
		Fields: []FieldMeta{
			{Name: "Title", Type: str, Tags: FieldTags{DBName: "title", JSONName: "title"}},
			{Name: "OwnerID", Type: str, Tags: FieldTags{DBName: "owner_id", JSONName: "ownerId"}},
			{Name: "Amount", Type: reflect.TypeOf(0), Tags: FieldTags{DBName: "amount", JSONName: "amount"}},
			{Name: "Resolved", Type: reflect.TypeOf(false), Tags: FieldTags{DBName: "resolved", JSONName: "resolved"}},
			{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}), Tags: FieldTags{DBName: "created_at", JSONName: "createdAt"}},
		},
	}
}

func buildFilterSQL(driver DriverType, filters []*FilterExpr) (string, []any) {
	pb := newAggPH(driver)
	return BuildFilterSQL(filterModel(), filters, driver, pb), pb.args
}

func f(field string, op FilterOperator, val any) *FilterExpr {
	return &FilterExpr{Field: field, Operator: op, Value: val}
}

func grouped(field string, op FilterOperator, val any, group int) *FilterExpr {
	return &FilterExpr{Field: field, Operator: op, Value: val, Group: group}
}

type filterCase struct {
	name     string
	driver   DriverType
	filters  []*FilterExpr
	wantSQL  string
	wantArgs []any
}

func TestBuildFilterSQL(t *testing.T) {
	t.Parallel()

	cases := []filterCase{
		// ── Nothing to render ────────────────────────────────────────────────
		{
			name:   "no_filters",
			driver: SQLite,
		},
		{
			name:    "nil_entries_are_skipped",
			driver:  SQLite,
			filters: []*FilterExpr{nil, f("title", OpEq, "a"), nil},
			// db/sqlcore's builder had no nil guard at all: it read f.Group off
			// a nil pointer and panicked, where the aggregate builder skipped.
			wantSQL:  `"tickets"."title" = ?`,
			wantArgs: []any{"a"},
		},

		// ── Comparison operators ─────────────────────────────────────────────
		{
			name:     "eq",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpEq, "a")},
			wantSQL:  `"tickets"."title" = ?`,
			wantArgs: []any{"a"},
		},
		{
			name:     "neq_gt_gte_lt_lte_are_anded",
			driver:   SQLite,
			filters:  []*FilterExpr{f("amount", OpNeq, 1), f("amount", OpGt, 2), f("amount", OpGte, 3), f("amount", OpLt, 4), f("amount", OpLte, 5)},
			wantSQL:  `"tickets"."amount" != ? AND "tickets"."amount" > ? AND "tickets"."amount" >= ? AND "tickets"."amount" < ? AND "tickets"."amount" <= ?`,
			wantArgs: []any{1, 2, 3, 4, 5},
		},
		{
			name:    "is_null_and_not_null_bind_nothing",
			driver:  SQLite,
			filters: []*FilterExpr{f("title", OpIsNull, ""), f("owner_id", OpNotNull, "")},
			wantSQL: `"tickets"."title" IS NULL AND "tickets"."owner_id" IS NOT NULL`,
		},

		// ── Column-to-column comparison (*_field) ────────────────────────────
		{
			name:    "eq_field_binds_no_parameter",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "amount", Operator: OpEqField, ValueField: "amount"}},
			wantSQL: `"tickets"."amount" = "tickets"."amount"`,
		},
		{
			name:    "gte_field_renders_both_columns",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "title", Operator: OpGteField, ValueField: "owner_id"}},
			wantSQL: `"tickets"."title" >= "tickets"."owner_id"`,
		},
		{
			name:    "gte_field_is_identical_on_postgres",
			driver:  Postgres,
			filters: []*FilterExpr{{Field: "title", Operator: OpGteField, ValueField: "owner_id"}},
			wantSQL: `"tickets"."title" >= "tickets"."owner_id"`,
		},
		{
			name:   "every_field_operator_renders_its_comparison",
			driver: SQLite,
			filters: []*FilterExpr{
				{Field: "amount", Operator: OpNeqField, ValueField: "amount"},
				{Field: "amount", Operator: OpGtField, ValueField: "amount"},
				{Field: "amount", Operator: OpGteField, ValueField: "amount"},
				{Field: "amount", Operator: OpLtField, ValueField: "amount"},
				{Field: "amount", Operator: OpLteField, ValueField: "amount"},
			},
			wantSQL: `"tickets"."amount" != "tickets"."amount" AND ` +
				`"tickets"."amount" > "tickets"."amount" AND ` +
				`"tickets"."amount" >= "tickets"."amount" AND ` +
				`"tickets"."amount" < "tickets"."amount" AND ` +
				`"tickets"."amount" <= "tickets"."amount"`,
		},
		{
			// The json spelling must resolve on both sides, exactly as it does
			// for the left side of an ordinary filter.
			name:    "field_operator_accepts_the_json_spelling",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "amount", Operator: OpGteField, ValueField: "ownerId"}},
			wantSQL: `"tickets"."amount" >= "tickets"."owner_id"`,
		},
		{
			// Fail closed: an unresolvable right-hand column must match nothing,
			// never vanish from the WHERE clause.
			name:    "field_operator_with_unknown_rhs_fails_closed",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "amount", Operator: OpGteField, ValueField: "nope"}},
			wantSQL: `1=0`,
		},
		{
			name:    "field_operator_with_empty_rhs_fails_closed",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "amount", Operator: OpGteField}},
			wantSQL: `1=0`,
		},
		{
			// A stray Value alongside ValueField must not become a bound
			// parameter — the right-hand side is the column, always.
			name:    "field_operator_ignores_a_stray_value",
			driver:  SQLite,
			filters: []*FilterExpr{{Field: "amount", Operator: OpGteField, ValueField: "amount", Value: "99"}},
			wantSQL: `"tickets"."amount" >= "tickets"."amount"`,
		},
		{
			name:   "field_operator_ors_within_a_group",
			driver: SQLite,
			filters: []*FilterExpr{
				{Field: "amount", Operator: OpGtField, ValueField: "amount", Group: 1},
				{Field: "title", Operator: OpEqField, ValueField: "owner_id", Group: 1},
			},
			wantSQL: `("tickets"."amount" > "tickets"."amount" OR "tickets"."title" = "tickets"."owner_id")`,
		},

		// ── IN / NOT IN, including the two builders' disagreements ───────────
		{
			name:     "in_csv",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpIn, "a,b")},
			wantSQL:  `"tickets"."title" IN (?, ?)`,
			wantArgs: []any{"a", "b"},
		},
		{
			name:   "in_go_slice",
			driver: SQLite,
			// The list builder bound fmt.Sprint([]string{"a","b"}) — the literal
			// "[a b]" — as a single value, so a Go-built IN matched nothing while
			// the same filter worked on the aggregate path.
			filters:  []*FilterExpr{f("title", OpIn, []string{"a", "b"})},
			wantSQL:  `"tickets"."title" IN (?, ?)`,
			wantArgs: []any{"a", "b"},
		},
		{
			name:     "in_any_slice",
			driver:   SQLite,
			filters:  []*FilterExpr{f("amount", OpIn, []any{1, 2})},
			wantSQL:  `"tickets"."amount" IN (?, ?)`,
			wantArgs: []any{1, 2},
		},
		{
			name:   "in_scalar",
			driver: SQLite,
			// The aggregate builder's splitter returned nil for anything that
			// was not a string or a slice, so a lone number became 1=0. Adopting
			// it wholesale would have been a regression; the merged splitter
			// falls back to the CSV form.
			filters:  []*FilterExpr{f("amount", OpIn, 42)},
			wantSQL:  `"tickets"."amount" IN (?)`,
			wantArgs: []any{"42"},
		},
		{
			name:    "in_empty_matches_nothing",
			driver:  SQLite,
			filters: []*FilterExpr{f("title", OpIn, "")},
			wantSQL: `1=0`,
		},
		{
			name:   "not_in_empty_matches_everything",
			driver: SQLite,
			// The decided winner of the second disagreement: an empty exclusion
			// set excludes nothing. The aggregate builder answered 1=0.
			filters: []*FilterExpr{f("title", OpNotIn, "")},
			wantSQL: `1=1`,
		},
		{
			name:     "not_in_csv",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpNotIn, "a,b")},
			wantSQL:  `"tickets"."title" NOT IN (?, ?)`,
			wantArgs: []any{"a", "b"},
		},

		// ── BETWEEN ──────────────────────────────────────────────────────────
		{
			name:     "between_expands_to_two_bounds",
			driver:   SQLite,
			filters:  []*FilterExpr{f("amount", OpBetween, "5,25")},
			wantSQL:  `("tickets"."amount" >= ? AND "tickets"."amount" <= ?)`,
			wantArgs: []any{"5", "25"},
		},
		{
			name:    "between_with_one_bound_fails_closed",
			driver:  SQLite,
			filters: []*FilterExpr{f("amount", OpBetween, "5")},
			wantSQL: `1=0`,
		},

		// ── Pattern operators, where the drivers diverge ─────────────────────
		{
			name:     "like",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpLike, "a%")},
			wantSQL:  `"tickets"."title" LIKE ?`,
			wantArgs: []any{"a%"},
		},
		{
			name:     "ilike_sqlite_lowers_both_sides",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpILike, "a%")},
			wantSQL:  `LOWER("tickets"."title") LIKE LOWER(?)`,
			wantArgs: []any{"a%"},
		},
		{
			name:     "ilike_postgres_is_native",
			driver:   Postgres,
			filters:  []*FilterExpr{f("title", OpILike, "a%")},
			wantSQL:  `"tickets"."title" ILIKE $1`,
			wantArgs: []any{"a%"},
		},
		{
			name:     "contains_sqlite_escapes",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpContains, "50%")},
			wantSQL:  `LOWER("tickets"."title") LIKE LOWER(?) ESCAPE '\'`,
			wantArgs: []any{`%50\%%`},
		},
		{
			name:     "contains_postgres_escapes",
			driver:   Postgres,
			filters:  []*FilterExpr{f("title", OpContains, "50%")},
			wantSQL:  `"tickets"."title" ILIKE $1 ESCAPE '\'`,
			wantArgs: []any{`%50\%%`},
		},
		{
			name:     "starts_with",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpStartsWith, "ab")},
			wantSQL:  `LOWER("tickets"."title") LIKE LOWER(?) ESCAPE '\'`,
			wantArgs: []any{`ab%`},
		},
		{
			name:     "ends_with",
			driver:   SQLite,
			filters:  []*FilterExpr{f("title", OpEndsWith, "ab")},
			wantSQL:  `LOWER("tickets"."title") LIKE LOWER(?) ESCAPE '\'`,
			wantArgs: []any{`%ab`},
		},

		// ── Fail-closed paths ────────────────────────────────────────────────
		{
			name:    "unknown_field_matches_nothing",
			driver:  SQLite,
			filters: []*FilterExpr{f("nope", OpEq, "x")},
			wantSQL: `1=0`,
		},
		{
			name:    "unimplemented_operator_matches_nothing",
			driver:  SQLite,
			filters: []*FilterExpr{f("title", FilterOperator("regex"), "x")},
			wantSQL: `1=0`,
		},

		// ── Field and value resolution ───────────────────────────────────────
		{
			name:     "json_name_resolves_to_its_column",
			driver:   SQLite,
			filters:  []*FilterExpr{f("ownerId", OpEq, "u1")},
			wantSQL:  `"tickets"."owner_id" = ?`,
			wantArgs: []any{"u1"},
		},
		{
			name:   "bool_word_is_coerced",
			driver: SQLite,
			// QF-3: "false" bound as TEXT against an INTEGER column matched zero
			// rows, with no error.
			filters:  []*FilterExpr{f("resolved", OpEq, "false")},
			wantSQL:  `"tickets"."resolved" = ?`,
			wantArgs: []any{false},
		},
		{
			name:     "time_is_written_canonically",
			driver:   SQLite,
			filters:  []*FilterExpr{f("createdAt", OpGt, time.Date(2026, 8, 3, 10, 30, 0, 0, time.UTC))},
			wantSQL:  `"tickets"."created_at" > ?`,
			wantArgs: []any{CanonicalTime(time.Date(2026, 8, 3, 10, 30, 0, 0, time.UTC))},
		},

		// ── Grouping ─────────────────────────────────────────────────────────
		{
			name:     "same_group_ors",
			driver:   SQLite,
			filters:  []*FilterExpr{grouped("title", OpEq, "a", 1), grouped("title", OpEq, "b", 1)},
			wantSQL:  `("tickets"."title" = ? OR "tickets"."title" = ?)`,
			wantArgs: []any{"a", "b"},
		},
		{
			name:   "distinct_groups_and_together_in_group_order",
			driver: SQLite,
			// Declared group 2 first: the builder must still emit group 1 first,
			// or the SQL depends on map iteration order and the same query
			// produces different statements run to run.
			filters: []*FilterExpr{
				grouped("title", OpEq, "a", 2), grouped("title", OpEq, "b", 2),
				grouped("amount", OpEq, 1, 1), grouped("amount", OpEq, 2, 1),
			},
			wantSQL:  `("tickets"."amount" = ? OR "tickets"."amount" = ?) AND ("tickets"."title" = ? OR "tickets"."title" = ?)`,
			wantArgs: []any{1, 2, "a", "b"},
		},
		{
			name:     "lone_group_needs_no_parentheses",
			driver:   SQLite,
			filters:  []*FilterExpr{grouped("title", OpEq, "a", 1)},
			wantSQL:  `"tickets"."title" = ?`,
			wantArgs: []any{"a"},
		},
		{
			name:     "ungrouped_ands_before_groups",
			driver:   SQLite,
			filters:  []*FilterExpr{f("amount", OpGt, 5), grouped("title", OpEq, "a", 1), grouped("title", OpEq, "b", 1)},
			wantSQL:  `"tickets"."amount" > ? AND ("tickets"."title" = ? OR "tickets"."title" = ?)`,
			wantArgs: []any{5, "a", "b"},
		},

		// ── Locale and nested columns ────────────────────────────────────────
		{
			name:   "locale_sqlite_binds_the_path",
			driver: SQLite,
			// The aggregate builder had no locale branch, so it compared the raw
			// JSON column instead of extracting the key.
			filters:  []*FilterExpr{{Field: "name", Operator: OpEq, Value: "x", IsLocale: true, LocaleKey: "ar"}},
			wantSQL:  `json_extract("tickets"."name", ?) = ?`,
			wantArgs: []any{"$.ar", "x"},
		},
		{
			name:     "locale_postgres_binds_the_key",
			driver:   Postgres,
			filters:  []*FilterExpr{{Field: "name", Operator: OpEq, Value: "x", IsLocale: true, LocaleKey: "ar"}},
			wantSQL:  `"tickets"."name"->>$1::text = $2`,
			wantArgs: []any{"ar", "x"},
		},
		{
			name:     "nested_uses_the_relation_alias",
			driver:   SQLite,
			filters:  []*FilterExpr{{Operator: OpEq, Value: "active", IsNested: true, RelationKey: "author", NestedField: "status"}},
			wantSQL:  `"author"."status" = ?`,
			wantArgs: []any{"active"},
		},

		// ── Placeholder numbering ────────────────────────────────────────────
		{
			name:     "postgres_numbers_placeholders_in_text_order",
			driver:   Postgres,
			filters:  []*FilterExpr{f("title", OpEq, "a"), f("amount", OpIn, "1,2")},
			wantSQL:  `"tickets"."title" = $1 AND "tickets"."amount" IN ($2, $3)`,
			wantArgs: []any{"a", "1", "2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSQL, gotArgs := buildFilterSQL(tc.driver, tc.filters)
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL:\n got %s\nwant %s", gotSQL, tc.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) && !(len(gotArgs) == 0 && len(tc.wantArgs) == 0) {
				t.Errorf("args: got %#v, want %#v", gotArgs, tc.wantArgs)
			}
		})
	}
}

// TestBuildFilterSQL_AggregateStillRefusesNested pins the one thing the
// aggregate path may not inherit from the shared builder. A nested filter
// renders as "relation"."column", which needs a JOIN the aggregate FROM clause
// does not emit — so the aggregate rejects it before the builder is reached
// rather than producing SQL that names a table it never joined.
func TestBuildFilterSQL_AggregateStillRefusesNested(t *testing.T) {
	t.Parallel()

	nested := &FilterExpr{Operator: OpEq, Value: "x", IsNested: true, RelationKey: "author", NestedField: "status"}
	_, err := aggBuildWhere(filterModel(), []*FilterExpr{nested}, SQLite, newAggPH(SQLite))
	if err == nil {
		t.Fatal("the aggregate path must refuse a nested filter, not render one")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("error must name the problem, got %q", err)
	}
}
