package maniflex

import (
	"reflect"
	"testing"
	"time"
)

// normModel is a model whose json and DB spellings deliberately differ, so a
// test can tell which one a lookup resolved by.
func normModel() *ModelMeta {
	return &ModelMeta{
		Name:      "Ticket",
		TableName: "tickets",
		Fields: []FieldMeta{
			{Name: "Title", Type: reflect.TypeOf(""),
				Tags: FieldTags{DBName: "title", JSONName: "title"}},
			{Name: "AuthorID", Type: reflect.TypeOf(""),
				Tags: FieldTags{DBName: "author_id", JSONName: "authorId"}},
			{Name: "Resolved", Type: reflect.TypeOf(false),
				Tags: FieldTags{DBName: "resolved", JSONName: "resolved"}},
			{Name: "ClosedAt", Type: reflect.TypeOf(&time.Time{}),
				Tags: FieldTags{DBName: "closed_at", JSONName: "closedAt"}},
			{Name: "CreatedAt", Type: reflect.TypeOf(time.Time{}),
				Tags: FieldTags{DBName: "created_at", JSONName: "createdAt"}},
		},
	}
}

// ── ResolveFilterField ────────────────────────────────────────────────────────

// A programmatic FilterExpr may name its field either way. ParseFilterParam has
// always accepted both spellings; before this, the Go-built path accepted only
// the DB one and sent the json name to the adapter verbatim, where it became an
// unknown column and a 500.
func TestResolveFilterField_AcceptsBothSpellings(t *testing.T) {
	m := normModel()
	for _, name := range []string{"author_id", "authorId"} {
		f := m.ResolveFilterField(name)
		if f == nil {
			t.Fatalf("ResolveFilterField(%q) = nil, want the AuthorID field", name)
		}
		if f.Tags.DBName != "author_id" {
			t.Fatalf("ResolveFilterField(%q) resolved to %q, want author_id", name, f.Tags.DBName)
		}
	}
}

func TestResolveFilterField_UnknownIsNil(t *testing.T) {
	if f := normModel().ResolveFilterField("nope"); f != nil {
		t.Fatalf("ResolveFilterField(nope) = %+v, want nil", f)
	}
}

// ── NormalizeFilterValue: time ────────────────────────────────────────────────

// The write path stores time.Time in CanonicalTimeLayout. A filter carrying a
// real time.Time used to bind as the driver's native form, which on SQLite is a
// different string than the one in the column — so the comparison was against a
// value that is not in the table (QF-2).
func TestNormalizeFilterValue_TimeValueCanonicalised(t *testing.T) {
	m := normModel()
	when := time.Date(2026, 8, 1, 12, 30, 45, 456_000_000, time.UTC)
	got := NormalizeFilterValue(m.ResolveFilterField("created_at"), OpLte, when)
	want := CanonicalTime(when)
	if got != want {
		t.Fatalf("time.Time value = %#v, want the canonical string %q", got, want)
	}
}

func TestNormalizeFilterValue_TimePointerCanonicalised(t *testing.T) {
	m := normModel()
	when := time.Date(2026, 8, 1, 12, 30, 45, 0, time.UTC)
	got := NormalizeFilterValue(m.ResolveFilterField("closed_at"), OpGte, &when)
	if got != CanonicalTime(when) {
		t.Fatalf("*time.Time value = %#v, want %q", got, CanonicalTime(when))
	}
}

// A zone offset must normalise to UTC, so it compares against the UTC strings
// the write path stores rather than sorting as its own local spelling.
func TestNormalizeFilterValue_TimeZoneNormalisedToUTC(t *testing.T) {
	m := normModel()
	zone := time.FixedZone("plus3", 3*60*60)
	when := time.Date(2026, 8, 1, 15, 0, 0, 0, zone)
	got := NormalizeFilterValue(m.ResolveFilterField("created_at"), OpGte, when)
	if got != CanonicalTime(when.UTC()) {
		t.Fatalf("zoned time = %#v, want %q", got, CanonicalTime(when.UTC()))
	}
}

// The existing string canonicalisation must keep working through the new entry
// point — this is what ParseFilterParam already relied on.
func TestNormalizeFilterValue_TimeStringStillCanonicalised(t *testing.T) {
	m := normModel()
	got := NormalizeFilterValue(m.ResolveFilterField("created_at"), OpGte, "2026-08-01T12:30:45Z")
	want := "2026-08-01T12:30:45.000000000Z"
	if got != want {
		t.Fatalf("time string = %#v, want %q", got, want)
	}
}

// A date-only bound has no time part to canonicalise and must survive untouched,
// or a ?filter=created_at:gte:2026-08-01 changes meaning.
func TestNormalizeFilterValue_DateOnlyUntouched(t *testing.T) {
	m := normModel()
	if got := NormalizeFilterValue(m.ResolveFilterField("created_at"), OpGte, "2026-08-01"); got != "2026-08-01" {
		t.Fatalf("date-only value = %#v, want it unchanged", got)
	}
}

// ── NormalizeFilterValue: bool ────────────────────────────────────────────────

// QF-3. A bool column binds a raw "false" as TEXT, which on SQLite's INTEGER
// column can never compare equal — the list comes back empty with no error.
func TestNormalizeFilterValue_BoolWordsCoerced(t *testing.T) {
	m := normModel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"false", false}, {"true", true},
		{"FALSE", false}, {"True", true},
		{"0", false}, {"1", true},
	} {
		got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpEq, tc.in)
		if got != tc.want {
			t.Fatalf("bool value %q = %#v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeFilterValue_BoolPassthrough(t *testing.T) {
	m := normModel()
	if got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpEq, true); got != true {
		t.Fatalf("real bool = %#v, want true", got)
	}
}

// A value that is not a boolean spelling is left alone rather than guessed at:
// coercing it would invent a predicate the caller did not write.
func TestNormalizeFilterValue_BoolGarbageUntouched(t *testing.T) {
	m := normModel()
	if got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpEq, "maybe"); got != "maybe" {
		t.Fatalf("non-boolean value = %#v, want it unchanged", got)
	}
}

// ── NormalizeFilterValue: set operators and edges ─────────────────────────────

// in/not_in/between carry a comma-separated list the adapters split with
// SplitCSV, so each element normalises independently and the value stays a
// joined string. A bool element therefore cannot become a Go bool the way a
// scalar one does — it becomes the numeral that binds correctly instead, since
// '1'/'0' convert under SQLite's numeric affinity where 'true'/'false' do not.
func TestNormalizeFilterValue_ListOperatorsNormaliseEachElement(t *testing.T) {
	m := normModel()
	got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpIn, "true,false")
	if got != "1,0" {
		t.Fatalf("bool list = %#v, want %q", got, "1,0")
	}

	a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	span := a.Format(time.RFC3339) + "," + b.Format(time.RFC3339)
	want := CanonicalTime(a) + "," + CanonicalTime(b)
	if got := NormalizeFilterValue(m.ResolveFilterField("created_at"), OpBetween, span); got != want {
		t.Fatalf("time between = %#v, want %q", got, want)
	}
}

// is_null / not_null bind nothing, so there is no value to coerce.
func TestNormalizeFilterValue_NullOperatorsUntouched(t *testing.T) {
	m := normModel()
	if got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpIsNull, nil); got != nil {
		t.Fatalf("is_null value = %#v, want nil", got)
	}
}

// An unresolved field cannot be coerced against anything; the caller decides
// what to do about it, and normalisation must not guess.
func TestNormalizeFilterValue_NilFieldUntouched(t *testing.T) {
	if got := NormalizeFilterValue(nil, OpEq, "false"); got != "false" {
		t.Fatalf("nil field = %#v, want the value unchanged", got)
	}
}

// Substring operators run against a text pattern; coercing their value to a
// bool would destroy the pattern.
func TestNormalizeFilterValue_SubstringOperatorsUntouched(t *testing.T) {
	m := normModel()
	if got := NormalizeFilterValue(m.ResolveFilterField("resolved"), OpContains, "true"); got != "true" {
		t.Fatalf("contains value = %#v, want it unchanged", got)
	}
}
