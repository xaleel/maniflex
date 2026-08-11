package maniflex

// filter_field_test.go covers the parse-time half of the *_field operators: the
// gauntlet a column-to-column filter has to clear before any SQL is built.
//
// The rendering half lives in filter_sql_test.go. The split matters because the
// renderer is deliberately permissive-then-fail-closed (an unknown column
// becomes 1=0), while the parser is where a client gets told *why* — an empty
// list and a 400 look very different to whoever is debugging.
//
// Run this group:
//
//	go test . -run TestFieldComparison

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// fieldCmpModel carries a filterable column of every comparability class, plus
// the three shapes that must be refused: non-filterable, encrypted, and a type
// with no ordering.
func fieldCmpModel() *ModelMeta {
	str := reflect.TypeOf("")
	i64 := reflect.TypeOf(int64(0))
	return &ModelMeta{
		Name:      "Order",
		TableName: "orders",
		Fields: []FieldMeta{
			{Name: "PaidAmount", Type: i64, Tags: FieldTags{DBName: "paid_amount", JSONName: "paid_amount", Filterable: true}},
			{Name: "AmountDue", Type: i64, Tags: FieldTags{DBName: "amount_due", JSONName: "amountDue", Filterable: true}},
			{Name: "Credit", Type: reflect.TypeOf((*int64)(nil)), Tags: FieldTags{DBName: "credit", JSONName: "credit", Filterable: true}},
			{Name: "Note", Type: str, Tags: FieldTags{DBName: "note", JSONName: "note", Filterable: true}},
			{Name: "Shipped", Type: reflect.TypeOf(false), Tags: FieldTags{DBName: "shipped", JSONName: "shipped", Filterable: true}},
			{Name: "PaidAt", Type: reflect.TypeOf(time.Time{}), Tags: FieldTags{DBName: "paid_at", JSONName: "paid_at", Filterable: true}},
			{Name: "Internal", Type: i64, Tags: FieldTags{DBName: "internal", JSONName: "internal"}},
			{Name: "Token", Type: str, Tags: FieldTags{DBName: "token", JSONName: "token", Filterable: true, Encrypted: true}},
			{Name: "Tags", Type: reflect.TypeOf([]string{}), Tags: FieldTags{DBName: "tags", JSONName: "tags", Filterable: true}},
		},
	}
}

func TestFieldComparisonParsesBothSides(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		raw            string
		wantField      string
		wantValueField string
		wantOp         FilterOperator
	}{
		{"db_names", "paid_amount:gte_field:amount_due", "paid_amount", "amount_due", OpGteField},
		{"json_name_on_the_right", "paid_amount:gte_field:amountDue", "paid_amount", "amount_due", OpGteField},
		{"json_name_on_the_left", "amountDue:lt_field:paid_amount", "amount_due", "paid_amount", OpLtField},
		{"eq", "paid_amount:eq_field:amount_due", "paid_amount", "amount_due", OpEqField},
		{"neq", "paid_amount:neq_field:amount_due", "paid_amount", "amount_due", OpNeqField},
		{"gt", "paid_amount:gt_field:amount_due", "paid_amount", "amount_due", OpGtField},
		{"lt", "paid_amount:lt_field:amount_due", "paid_amount", "amount_due", OpLtField},
		{"lte", "paid_amount:lte_field:amount_due", "paid_amount", "amount_due", OpLteField},
		{"nullable_against_plain", "credit:gte_field:paid_amount", "credit", "paid_amount", OpGteField},
		{"strings", "note:eq_field:note", "note", "note", OpEqField},
		{"bools", "shipped:eq_field:shipped", "shipped", "shipped", OpEqField},
		{"times", "paid_at:gte_field:paid_at", "paid_at", "paid_at", OpGteField},
		{"surrounding_space_is_trimmed", "paid_amount:gte_field: amount_due ", "paid_amount", "amount_due", OpGteField},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFilterParam(tc.raw, fieldCmpModel(), nil)
			if err != nil {
				t.Fatalf("ParseFilterParam(%q) returned error: %v", tc.raw, err)
			}
			if got.Field != tc.wantField {
				t.Errorf("Field = %q, want %q", got.Field, tc.wantField)
			}
			if got.ValueField != tc.wantValueField {
				t.Errorf("ValueField = %q, want %q", got.ValueField, tc.wantValueField)
			}
			if got.Operator != tc.wantOp {
				t.Errorf("Operator = %q, want %q", got.Operator, tc.wantOp)
			}
			// Value must be cleared: anything left there could reach a binder.
			if got.Value != nil {
				t.Errorf("Value = %#v, want nil", got.Value)
			}
		})
	}
}

func TestFieldComparisonRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		wantMsg string
	}{
		{"unknown_right_column", "paid_amount:gte_field:nope", `"nope" not found`},
		{"unknown_left_column", "nope:gte_field:paid_amount", `"nope" not found`},
		{"right_column_not_filterable", "paid_amount:gte_field:internal", "not filterable"},
		{"left_column_not_filterable", "internal:gte_field:paid_amount", "not filterable"},
		{"encrypted_right", "paid_amount:eq_field:token", "encrypted"},
		{"encrypted_left", "token:eq_field:note", "encrypted"},
		{"number_against_string", "paid_amount:gte_field:note", "same kind of value"},
		{"bool_against_number", "shipped:eq_field:paid_amount", "same kind of value"},
		{"time_against_string", "paid_at:gte_field:note", "same kind of value"},
		{"unorderable_type", "tags:eq_field:tags", "no ordering"},
		{"missing_right_side", "paid_amount:gte_field", "requires a field name"},
		{"empty_right_side", "paid_amount:gte_field:", "requires a field name"},
		{"blank_right_side", "paid_amount:gte_field:   ", "requires a field name"},
		{"dotted_right_side", "paid_amount:gte_field:customer.credit", "not supported"},
		{"dotted_left_side", "customer.credit:gte_field:paid_amount", "not supported"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseFilterParam(tc.raw, fieldCmpModel(), nil)
			if err == nil {
				t.Fatalf("ParseFilterParam(%q) succeeded, want an error", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateFilterFieldsChecksValueField(t *testing.T) {
	t.Parallel()

	model := fieldCmpModel()

	t.Run("resolvable_value_field_passes", func(t *testing.T) {
		t.Parallel()
		fs := []*FilterExpr{{Field: "paid_amount", Operator: OpGteField, ValueField: "amount_due"}}
		if err := validateFilterFields(model, fs); err != nil {
			t.Fatalf("validateFilterFields returned error: %v", err)
		}
	})

	t.Run("json_spelling_of_value_field_passes", func(t *testing.T) {
		t.Parallel()
		fs := []*FilterExpr{{Field: "paid_amount", Operator: OpGteField, ValueField: "amountDue"}}
		if err := validateFilterFields(model, fs); err != nil {
			t.Fatalf("validateFilterFields returned error: %v", err)
		}
	})

	t.Run("unknown_value_field_is_named", func(t *testing.T) {
		t.Parallel()
		fs := []*FilterExpr{{Field: "paid_amount", Operator: OpGteField, ValueField: "nope"}}
		err := validateFilterFields(model, fs)
		if err == nil {
			t.Fatal("validateFilterFields accepted an unknown ValueField")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error = %q, want it to name the field", err.Error())
		}
	})

	t.Run("unknown_left_field_still_reported", func(t *testing.T) {
		t.Parallel()
		fs := []*FilterExpr{{Field: "nope", Operator: OpGteField, ValueField: "amount_due"}}
		if err := validateFilterFields(model, fs); err == nil {
			t.Fatal("validateFilterFields accepted an unknown Field")
		}
	})

	t.Run("ordinary_filters_are_unaffected", func(t *testing.T) {
		t.Parallel()
		fs := []*FilterExpr{{Field: "paid_amount", Operator: OpGte, Value: 10}}
		if err := validateFilterFields(model, fs); err != nil {
			t.Fatalf("validateFilterFields returned error: %v", err)
		}
	})
}

func TestFieldTypeClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		typ  reflect.Type
		want filterTypeClass
	}{
		{"int", reflect.TypeOf(0), classNumber},
		{"int64", reflect.TypeOf(int64(0)), classNumber},
		{"uint32", reflect.TypeOf(uint32(0)), classNumber},
		{"float64", reflect.TypeOf(0.0), classNumber},
		{"pointer_int64", reflect.TypeOf((*int64)(nil)), classNumber},
		{"string", reflect.TypeOf(""), classString},
		{"pointer_string", reflect.TypeOf((*string)(nil)), classString},
		{"bool", reflect.TypeOf(false), classBool},
		{"pointer_bool", reflect.TypeOf((*bool)(nil)), classBool},
		// time.Time is a struct, so it has to be settled before the Kind switch
		// or it would fall through to classNone.
		{"time", reflect.TypeOf(time.Time{}), classTime},
		{"pointer_time", reflect.TypeOf((*time.Time)(nil)), classTime},
		{"slice", reflect.TypeOf([]string{}), classNone},
		{"map", reflect.TypeOf(map[string]string{}), classNone},
		{"nil", nil, classNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fieldTypeClass(tc.typ); got != tc.want {
				t.Errorf("fieldTypeClass(%v) = %q, want %q", tc.typ, got, tc.want)
			}
		})
	}
}
