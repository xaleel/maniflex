package maniflex

// NEW-1 (the db/sqlcore sibling of jobs/sql audit JB-7): SQLite stores timestamps
// as TEXT and compares them byte-by-byte, so the string form maniflex writes — and
// canonicalises filter values into — must sort lexicographically the same way the
// instants sort. These tests pin that property and the value-canonicalisation rules
// the filter path relies on. SQLite's BINARY collation is byte-identical to Go
// string comparison for these ASCII strings, so the pure comparisons here are a
// faithful stand-in; the functional path is covered end-to-end in tests/e2e.
//
//	go test . -run TestCanonical

import (
	"reflect"
	"testing"
	"time"
)

// The property every SQLite range comparison relies on: lexicographic order of
// CanonicalTime equals chronological order, including across the whole-second /
// fractional boundary that time.RFC3339Nano gets wrong.
func TestCanonicalTime_LexicographicOrderMatchesTime(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC)
	times := []time.Time{
		base.Add(-1 * time.Nanosecond),        // 55.999999999
		base,                                  // 56.000000000 (whole second)
		base.Add(1 * time.Nanosecond),         // 56.000000001
		base.Add(500 * time.Millisecond),      // 56.500000000
		base.Add(999999999 * time.Nanosecond), // 56.999999999
		base.Add(time.Second),                 // 57.000000000
	}
	for _, a := range times {
		for _, b := range times {
			lexLE := CanonicalTime(a) <= CanonicalTime(b)
			timeLE := !a.After(b)
			if lexLE != timeLE {
				t.Errorf("CanonicalTime(%s)=%q vs CanonicalTime(%s)=%q: lexicographic <= is %v but chronological <= is %v",
					a.Format(time.RFC3339Nano), CanonicalTime(a),
					b.Format(time.RFC3339Nano), CanonicalTime(b), lexLE, timeLE)
			}
		}
	}
}

// The fraction is a fixed nine digits, which is what keeps every stamp the same
// width. A regression back to RFC3339Nano would drop the trailing zeros.
func TestCanonicalTime_FixedWidth(t *testing.T) {
	got := CanonicalTime(time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC))
	want := "2026-07-21T12:34:56.000000000Z"
	if got != want {
		t.Errorf("CanonicalTime(whole second) = %q, want %q", got, want)
	}
	if l := len(CanonicalTime(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC))); l != len(want) {
		t.Errorf("CanonicalTime length = %d, want the fixed %d", l, len(want))
	}
}

// canonicalTimeValue normalises a full RFC3339 timestamp (with or without a
// fraction, with or without a zone offset) to the fixed-width UTC form, and reports
// ok=false — leave it alone — for anything that is not a full timestamp.
func TestCanonicalTimeValue(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"2026-07-21T12:00:00Z", "2026-07-21T12:00:00.000000000Z", true},      // no fraction → padded
		{"2026-07-21T12:00:00.5Z", "2026-07-21T12:00:00.500000000Z", true},    // short fraction → padded
		{"2026-07-21T17:00:00+05:00", "2026-07-21T12:00:00.000000000Z", true}, // offset → normalised to UTC
		{"2026-07-21", "", false}, // date-only: not a full timestamp, pass through
		{"draft", "", false},      // non-time string
		{"", "", false},           // empty
	}
	for _, c := range cases {
		got, ok := canonicalTimeValue(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("canonicalTimeValue(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// The value handed to a between/in filter is a comma-separated list; each element
// that is a full timestamp is canonicalised independently, the rest pass through.
func TestCanonicalizeTimeFilterValue(t *testing.T) {
	// Scalar: a well-formed bound is canonicalised.
	if got := canonicalizeTimeFilterValue(OpGte, "2026-07-21T12:00:00Z"); got != "2026-07-21T12:00:00.000000000Z" {
		t.Errorf("scalar gte = %q", got)
	}
	// Scalar: a date-only bound is left exactly as sent.
	if got := canonicalizeTimeFilterValue(OpLte, "2026-07-21"); got != "2026-07-21" {
		t.Errorf("scalar date-only = %q, want unchanged", got)
	}
	// Between: both bounds canonicalised.
	if got := canonicalizeTimeFilterValue(OpBetween, "2026-07-21T00:00:00Z,2026-07-22T00:00:00Z"); got != "2026-07-21T00:00:00.000000000Z,2026-07-22T00:00:00.000000000Z" {
		t.Errorf("between = %q", got)
	}
	// In: a timestamp element is canonicalised, a non-timestamp element is not.
	if got := canonicalizeTimeFilterValue(OpIn, "2026-07-21T12:00:00Z,never"); got != "2026-07-21T12:00:00.000000000Z,never" {
		t.Errorf("in mixed = %q", got)
	}
	// A non-string value (a hand-built FilterExpr may carry any) is untouched.
	if got := canonicalizeTimeFilterValue(OpGte, 123); got != 123 {
		t.Errorf("non-string = %v, want unchanged", got)
	}
}

func TestIsTimeType(t *testing.T) {
	var tt time.Time
	var ptt *time.Time
	yes := []reflect.Type{reflect.TypeOf(tt), reflect.TypeOf(ptt)}
	for _, ty := range yes {
		if !isTimeType(ty) {
			t.Errorf("isTimeType(%v) = false, want true", ty)
		}
	}
	no := []reflect.Type{reflect.TypeOf(""), reflect.TypeOf(0), nil}
	for _, ty := range no {
		if isTimeType(ty) {
			t.Errorf("isTimeType(%v) = true, want false", ty)
		}
	}
}
