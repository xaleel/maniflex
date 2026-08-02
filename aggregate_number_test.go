package maniflex

// AG-3 — a numeric aggregate must reach the client as a JSON number on every
// driver. lib/pq scans a Postgres NUMERIC (what SUM over a BIGINT, and AVG over
// anything, come back as) into bytes the row scanner turns into a Go string,
// while SQLite returns an int64 or float64 — so the same query produced
// {"sum":"150"} on one driver and {"sum":150} on the other.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func numModel() *ModelMeta {
	return &ModelMeta{
		Name:      "Sale",
		TableName: "sales",
		Fields: []FieldMeta{
			{Name: "Amount", Type: reflect.TypeOf(0),
				Tags: FieldTags{DBName: "amount", JSONName: "amount"}},
			{Name: "Rate", Type: reflect.TypeOf(0.0),
				Tags: FieldTags{DBName: "rate", JSONName: "rate"}},
			{Name: "Sku", Type: reflect.TypeOf(""),
				Tags: FieldTags{DBName: "sku", JSONName: "sku"}},
		},
	}
}

// ── which aggregates are numeric ──────────────────────────────────────────────

func TestAggregateYieldsNumber(t *testing.T) {
	m := numModel()
	cases := []struct {
		name string
		f    AggregateField
		want bool
	}{
		{"count(*)", AggregateField{Op: AggCount}, true},
		{"count(col)", AggregateField{Op: AggCount, Field: "sku"}, true},
		{"count_distinct", AggregateField{Op: AggCountDistinct, Field: "sku"}, true},
		{"sum", AggregateField{Op: AggSum, Field: "amount"}, true},
		{"avg", AggregateField{Op: AggAvg, Field: "rate"}, true},
		{"min numeric", AggregateField{Op: AggMin, Field: "amount"}, true},
		{"max numeric", AggregateField{Op: AggMax, Field: "rate"}, true},

		// MIN/MAX return the operand's own type. Over a text column that is
		// text, and coercing a value like "00123" to a number would corrupt it.
		{"min text", AggregateField{Op: AggMin, Field: "sku"}, false},
		{"max text", AggregateField{Op: AggMax, Field: "sku"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateYieldsNumber(m, tc.f); got != tc.want {
				t.Fatalf("aggregateYieldsNumber = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── the JSON-number guard ─────────────────────────────────────────────────────

func TestIsJSONNumber(t *testing.T) {
	valid := []string{"0", "-0", "150", "-150", "1.5", "-1.5", "0.5",
		"1e5", "1E5", "1e+5", "1e-5", "1.5e10", "12345678901234567890"}
	for _, s := range valid {
		if !isJSONNumber(s) {
			t.Errorf("isJSONNumber(%q) = false, want true", s)
		}
	}

	// Postgres NUMERIC also admits NaN and Infinity, and neither is a JSON
	// number — emitting one as a bare token would produce a document no parser
	// accepts. Leading zeros are rejected for the same reason: encoding/json
	// refuses to marshal a json.Number that is not valid JSON.
	invalid := []string{"", "NaN", "Infinity", "-Infinity", "nan", "inf",
		"007", "-007", "1.", ".5", "1e", "1e+", "abc", "1.2.3", "150 ", " 150", "0x10", "+150"}
	for _, s := range invalid {
		if isJSONNumber(s) {
			t.Errorf("isJSONNumber(%q) = true, want false", s)
		}
	}
}

// Whatever isJSONNumber accepts, encoding/json must be willing to marshal — a
// disagreement there turns a report into a 500.
//
// The empty string is excluded deliberately, and is the reason this check
// cannot simply be "try json.Marshal and see": encoding/json substitutes "0"
// for an empty json.Number and marshals it happily. Deferring to the encoder
// would therefore turn a blank value into a total of zero, which is worse than
// the string it replaced.
func TestIsJSONNumberAgreesWithEncodingJSON(t *testing.T) {
	for _, s := range []string{"0", "-0", "150", "1.5", "1e5", "1E5", "1e+5",
		"1e-5", "12345678901234567890", "NaN", "Infinity", "007", "1.", ".5", "abc"} {
		_, err := json.Marshal(json.Number(s))
		if ok := isJSONNumber(s); ok != (err == nil) {
			t.Errorf("isJSONNumber(%q) = %v but json.Marshal err = %v", s, ok, err)
		}
	}

	if _, err := json.Marshal(json.Number("")); err != nil {
		t.Fatalf("premise changed: empty json.Number now fails to marshal (%v)", err)
	}
	if isJSONNumber("") {
		t.Fatal("an empty value must not be converted — it would serialise as 0")
	}
}

// ── normalisation ─────────────────────────────────────────────────────────────

func TestNormalizeAggregateNumbers(t *testing.T) {
	rows := []Row{{
		"sum_amount": "150",     // Postgres NUMERIC, arrives as a string
		"avg_rate":   "1.5",     // ditto
		"count":      int64(3),  // already a number, left alone
		"min_sku":    "00123",   // not in the numeric set — must stay a string
		"sku":        "42",      // a group_by column — never converted
		"sum_empty":  nil,       // SUM over no rows
	}}
	numeric := map[string]bool{"sum_amount": true, "avg_rate": true, "count": true, "sum_empty": true}
	normalizeAggregateNumbers(rows, numeric)

	if got := rows[0]["sum_amount"]; got != json.Number("150") {
		t.Errorf("sum_amount = %#v, want json.Number(\"150\")", got)
	}
	if got := rows[0]["avg_rate"]; got != json.Number("1.5") {
		t.Errorf("avg_rate = %#v, want json.Number(\"1.5\")", got)
	}
	if got := rows[0]["count"]; got != int64(3) {
		t.Errorf("count = %#v, want it left as int64", got)
	}
	if got := rows[0]["min_sku"]; got != "00123" {
		t.Errorf("min_sku = %#v — a non-numeric aggregate must keep its string", got)
	}
	if got := rows[0]["sku"]; got != "42" {
		t.Errorf("sku = %#v — a group_by column must never be converted", got)
	}
	if rows[0]["sum_empty"] != nil {
		t.Errorf("sum_empty = %#v, want nil", rows[0]["sum_empty"])
	}
}

// A NUMERIC that is not a JSON number stays a string rather than becoming a
// token that breaks serialisation for the whole response.
func TestNormalizeAggregateNumbers_LeavesNaN(t *testing.T) {
	rows := []Row{{"sum_amount": "NaN"}}
	normalizeAggregateNumbers(rows, map[string]bool{"sum_amount": true})
	if got := rows[0]["sum_amount"]; got != "NaN" {
		t.Fatalf("sum_amount = %#v, want the string NaN left alone", got)
	}
}

func TestNormalizeAggregateNumbers_HandlesBytes(t *testing.T) {
	rows := []Row{{"sum_amount": []byte("150")}}
	normalizeAggregateNumbers(rows, map[string]bool{"sum_amount": true})
	if got := rows[0]["sum_amount"]; got != json.Number("150") {
		t.Fatalf("sum_amount = %#v, want json.Number(\"150\")", got)
	}
}

// ── serialisation ─────────────────────────────────────────────────────────────

// The whole point is the bytes on the wire: json.Number must serialise as a
// bare number, and must keep digits a float64 would round away.
func TestJSONNumberSerialisesUnquoted(t *testing.T) {
	rec := httptest.NewRecorder()
	(&APIResponse{
		StatusCode: http.StatusOK,
		Data: []Row{{
			"sum_amount": json.Number("150"),
			"big":        json.Number("12345678901234567890"),
		}},
	}).Write(rec)

	body := rec.Body.String()
	if !strings.Contains(body, `"sum_amount":150`) {
		t.Fatalf("want an unquoted number in the body, got: %s", body)
	}
	if !strings.Contains(body, `"big":12345678901234567890`) {
		t.Fatalf("a wide total must keep its exact digits, got: %s", body)
	}
}
