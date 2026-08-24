package maniflex

// The parse-side half of the JSON filter operators (todo/ASKS.md issue 1).
//
// Two directions have to be closed, not one. `has` on a column that holds no
// JSON has no meaning and would reach the builder as a false predicate — a
// filter that silently matches nothing. And `contains` on a column that does
// hold JSON is the original bug: a substring LIKE across a serialised document,
// which matched across element boundaries on SQLite and could not run at all on
// Postgres. Refusing both at parse time means neither shape ever becomes a query.

import (
	"reflect"
	"strings"
	"testing"
)

func jsonFilterModel() *ModelMeta {
	str := reflect.TypeOf("")
	return &ModelMeta{
		Name:      "Merchant",
		TableName: "merchants",
		Fields: []FieldMeta{
			{Name: "Name", Type: str, Tags: FieldTags{
				DBName: "name", JSONName: "name", Filterable: true,
			}},
			{Name: "CategoryIDs", Type: reflect.TypeOf([]string(nil)), Tags: FieldTags{
				DBName: "category_ids", JSONName: "categoryIds", Filterable: true, JSONArray: true,
			}},
			{Name: "Meta", Type: reflect.TypeOf(map[string]any(nil)), Tags: FieldTags{
				DBName: "meta", JSONName: "meta", Filterable: true, JSONObject: true,
			}},
		},
	}
}

func TestParseFilter_HasRequiresAJSONColumn(t *testing.T) {
	m := jsonFilterModel()

	t.Run("accepted on a json_array column", func(t *testing.T) {
		expr, err := ParseFilterParam("categoryIds:has:cat-1", m, nil)
		if err != nil {
			t.Fatalf("rejected has on a json_array column: %v", err)
		}
		if expr.Field != "category_ids" || expr.Operator != OpHas {
			t.Errorf("parsed to %+v", expr)
		}
	})

	t.Run("accepted on a json_object column", func(t *testing.T) {
		if _, err := ParseFilterParam("meta:has:name=John", m, nil); err != nil {
			t.Fatalf("rejected has on a json_object column: %v", err)
		}
	})

	t.Run("refused on a plain column", func(t *testing.T) {
		_, err := ParseFilterParam("name:has:John", m, nil)
		if err == nil {
			t.Fatal("has was accepted on a plain text column")
		}
		// The message has to name the way out. A developer who reaches for `has`
		// has a JSON column in mind and needs to know the tag exists.
		for _, want := range []string{"name", "json_array", "json_object"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("not_has is held to the same rule", func(t *testing.T) {
		if _, err := ParseFilterParam("name:not_has:John", m, nil); err == nil {
			t.Fatal("not_has was accepted on a plain text column")
		}
	})
}

// The original defect: contains against a JSON column is a substring match over
// a serialised document. It answered 200 with wrong rows on SQLite and could not
// run on Postgres, so no caller can be relying on it being correct.
func TestParseFilter_SubstringOperatorsRefusedOnJSONColumns(t *testing.T) {
	m := jsonFilterModel()
	for _, op := range []FilterOperator{OpContains, OpStartsWith, OpEndsWith} {
		for _, field := range []string{"categoryIds", "meta"} {
			raw := field + ":" + string(op) + ":cat-1"
			_, err := ParseFilterParam(raw, m, nil)
			if err == nil {
				t.Errorf("%q was accepted; it substring-matches the serialised JSON", raw)
				continue
			}
			if !strings.Contains(err.Error(), "has") {
				t.Errorf("%q: error does not point at the operator that works: %v", raw, err)
			}
		}
	}
	// Every other operator is left alone: eq against the whole document is a
	// coherent question, and refusing it would be a change nobody asked for.
	if _, err := ParseFilterParam("meta:is_null:", m, nil); err != nil {
		t.Errorf("is_null was refused on a JSON column: %v", err)
	}
}

// The object key reaches SQLite inside a JSON path. It is bound rather than
// spliced, but it is still held to a rule at the door — the same belt-and-braces
// the locale key gets (SEC-1).
func TestParseFilter_HasObjectKeyIsValidated(t *testing.T) {
	m := jsonFilterModel()
	for _, tc := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{"plain key", "meta:has:name=John", true},
		{"underscored key", "meta:has:first_name=John", true},
		{"value may hold an equals sign", "meta:has:expr=a=b", true},
		{"no pair", "meta:has:name", false},
		{"empty key", "meta:has:=John", false},
		{"quote in key", `meta:has:na"me=John`, false},
		{"a path is not supported yet", "meta:has:a.b=John", false},
	} {
		_, err := ParseFilterParam(tc.raw, m, nil)
		if tc.valid && err != nil {
			t.Errorf("%s: %q rejected: %v", tc.name, tc.raw, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("%s: %q accepted", tc.name, tc.raw)
		}
	}
}

// An array element is a whole value, so there is no key to validate — but an
// empty one is a filter with nothing in it, which every other value-taking
// operator here refuses.
func TestParseFilter_HasRequiresAValue(t *testing.T) {
	m := jsonFilterModel()
	if _, err := ParseFilterParam("categoryIds:has:", m, nil); err == nil {
		t.Error("has with no value was accepted")
	}
}
