package sqlcore

// `has` asks whether a JSON column holds a value: an element of a `json_array`
// column, or a key/value pair of a `json_object` one.
//
// It exists because `contains` was the only operator that looked like it would
// answer that, and it compiles to a substring LIKE. Against a JSON array that is
// wrong in a different way per driver — SQLite matches across element boundaries
// (`cat-1` finds `["cat-10"]`, HTTP 200) while Postgres cannot apply LIKE to
// JSONB at all — so the query that appeared to work in development returned
// silently incorrect rows there and failed outright in production.
//
// Both drivers therefore have to answer the same question here, and both compare
// the JSON encoding of the value rather than its text: a bare `5` and the string
// `"5"` are different elements of a JSON array, and a filter that conflated them
// would be the same class of bug in miniature.

import (
	"reflect"
	"testing"

	"github.com/xaleel/maniflex"
)

func TestFilterConds_Has_ArrayMembership(t *testing.T) {
	cases := []struct {
		driver maniflex.DriverType
		want   string
		arg    any
	}{
		{
			maniflex.Postgres,
			`"posts"."tags" @> $1::jsonb`,
			`"cat-1"`,
		},
		{
			maniflex.SQLite,
			`EXISTS (SELECT 1 FROM json_each("posts"."tags") WHERE json_quote("json_each"."value") = ?)`,
			`"cat-1"`,
		},
	}
	for _, tc := range cases {
		sql, args := filterCondsSQL(tc.driver, jsonModel(), []*maniflex.FilterExpr{
			f("tags", maniflex.OpHas, "cat-1", -1),
		})
		if sql != tc.want {
			t.Errorf("%v sql:\n got %q\nwant %q", tc.driver, sql, tc.want)
		}
		if len(args) != 1 || args[0] != tc.arg {
			t.Errorf("%v args: got %#v, want [%v] — the element is compared JSON-encoded, "+
				"so a string element cannot match a numeric one", tc.driver, args, tc.arg)
		}
	}
}

// not_has is the negation, and exists because every other set-shaped operator
// here comes in a pair (in/not_in, is_null/not_null). Scoping a list to exclude a
// tag is as ordinary as scoping it to include one.
func TestFilterConds_NotHas(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.Postgres, maniflex.SQLite} {
		sql, _ := filterCondsSQL(driver, jsonModel(), []*maniflex.FilterExpr{
			f("tags", maniflex.OpNotHas, "cat-1", -1),
		})
		pos, _ := filterCondsSQL(driver, jsonModel(), []*maniflex.FilterExpr{
			f("tags", maniflex.OpHas, "cat-1", -1),
		})
		if want := "NOT (" + pos + ")"; sql != want {
			t.Errorf("%v sql:\n got %q\nwant %q", driver, sql, want)
		}
	}
}

// On a json_object column the value is key=value, and the pair is matched as a
// document so Postgres can serve it from the same GIN index array membership uses.
func TestFilterConds_Has_ObjectPair(t *testing.T) {
	cases := []struct {
		driver maniflex.DriverType
		want   string
		args   []any
	}{
		{
			maniflex.Postgres,
			`"posts"."meta" @> $1::jsonb`,
			[]any{`{"name":"John"}`},
		},
		{
			maniflex.SQLite,
			`json_quote(json_extract("posts"."meta", ?)) = ?`,
			[]any{`$."name"`, `"John"`},
		},
	}
	for _, tc := range cases {
		expr := f("meta", maniflex.OpHas, "name=John", -1)
		sql, args := filterCondsSQL(tc.driver, jsonModel(), []*maniflex.FilterExpr{expr})
		if sql != tc.want {
			t.Errorf("%v sql:\n got %q\nwant %q", tc.driver, sql, tc.want)
		}
		if len(args) != len(tc.args) {
			t.Fatalf("%v args: got %#v, want %#v", tc.driver, args, tc.args)
		}
		for i := range args {
			if args[i] != tc.args[i] {
				t.Errorf("%v arg %d: got %#v, want %#v", tc.driver, i, args[i], tc.args[i])
			}
		}
	}
}

// A value the column cannot hold must not become a predicate that matches
// everything. filterPredicate's whole convention is to fail closed.
func TestFilterConds_Has_MalformedObjectPairFailsClosed(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.Postgres, maniflex.SQLite} {
		sql, _ := filterCondsSQL(driver, jsonModel(), []*maniflex.FilterExpr{
			f("meta", maniflex.OpHas, "no-equals-sign", -1),
		})
		// "1=0" — filter_sql.go's falsePredicate, unexported there.
		if sql != "1=0" {
			t.Errorf("%v: a json_object has with no key=value rendered %q, want 1=0", driver, sql)
		}
	}
}

// jsonModel carries one json_array column and one json_object column, tagged as
// a model author would tag them.
func jsonModel() *maniflex.ModelMeta {
	m := postModel()
	m.Fields = append(m.Fields,
		maniflex.FieldMeta{
			Name: "Tags", Type: reflect.TypeOf([]string(nil)),
			Tags: maniflex.FieldTags{
				DBName: "tags", JSONName: "tags", Filterable: true, JSONArray: true,
			},
		},
		maniflex.FieldMeta{
			Name: "Meta", Type: reflect.TypeOf(map[string]any(nil)),
			Tags: maniflex.FieldTags{
				DBName: "meta", JSONName: "meta", Filterable: true, JSONObject: true,
			},
		},
	)
	return m
}

// The trap in negation: NOT (1=0) is 1=1. A not_has the builder cannot render
// must fail closed like every other unrenderable filter, not invert into a
// predicate that matches every row — which on a forced filter is a lost scope.
func TestFilterConds_NotHas_MalformedFailsClosedRatherThanInverting(t *testing.T) {
	for _, driver := range []maniflex.DriverType{maniflex.Postgres, maniflex.SQLite} {
		for _, tc := range []struct{ name, value string }{
			{"no key=value", "no-equals-sign"},
			{"empty key", "=John"},
			{"quote in key", `na"me=John`},
		} {
			sql, _ := filterCondsSQL(driver, jsonModel(), []*maniflex.FilterExpr{
				f("meta", maniflex.OpNotHas, tc.value, -1),
			})
			if sql != "1=0" {
				t.Errorf("%v %s: rendered %q, want 1=0 — negating the false predicate "+
					"matches every row", driver, tc.name, sql)
			}
		}
	}
}
