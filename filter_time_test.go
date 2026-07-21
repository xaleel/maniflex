package maniflex_test

// NEW-1: a filter value on a time-typed column must be canonicalised to the
// fixed-width form the write path stores, so ?filter=created_at:gte:<ts> orders
// correctly against stored rows on SQLite. A value on a non-time column, and a
// date-only value, must be passed through untouched.

import (
	"reflect"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

func timeFilterModel() *maniflex.ModelMeta {
	return &maniflex.ModelMeta{
		Name:      "Event",
		TableName: "events",
		Fields: []maniflex.FieldMeta{
			{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}),
				Tags: maniflex.FieldTags{DBName: "created_at", JSONName: "created_at", Filterable: true}},
			{Name: "Name", Type: reflect.TypeOf(""),
				Tags: maniflex.FieldTags{DBName: "name", JSONName: "name", Filterable: true}},
		},
	}
}

func parseFilter(t *testing.T, raw string) *maniflex.FilterExpr {
	t.Helper()
	expr, err := maniflex.ParseFilterParam(raw, timeFilterModel(), nil)
	if err != nil {
		t.Fatalf("ParseFilterParam(%q): %v", raw, err)
	}
	return expr
}

func TestParseFilter_TimeFieldCanonicalised(t *testing.T) {
	// A whole-second bound, as a client would send it, is padded to fixed width.
	expr := parseFilter(t, "created_at:gte:2026-07-21T12:00:00Z")
	if expr.Value != "2026-07-21T12:00:00.000000000Z" {
		t.Errorf("time filter value = %q, want fixed-width canonical form", expr.Value)
	}

	// A zone offset is normalised to UTC so it compares against stored UTC strings.
	expr = parseFilter(t, "created_at:lt:2026-07-21T17:00:00+05:00")
	if expr.Value != "2026-07-21T12:00:00.000000000Z" {
		t.Errorf("offset filter value = %q, want normalised to UTC", expr.Value)
	}
}

func TestParseFilter_NonTimeFieldUntouched(t *testing.T) {
	// A value that happens to look like a timestamp on a string column is left as-is.
	expr := parseFilter(t, "name:eq:2026-07-21T12:00:00Z")
	if expr.Value != "2026-07-21T12:00:00Z" {
		t.Errorf("non-time filter value = %q, want unchanged", expr.Value)
	}
}

func TestParseFilter_DateOnlyBoundUntouched(t *testing.T) {
	// A date-only bound is not a full timestamp; its existing meaning is preserved.
	expr := parseFilter(t, "created_at:gte:2026-07-21")
	if expr.Value != "2026-07-21" {
		t.Errorf("date-only filter value = %q, want unchanged", expr.Value)
	}
}
