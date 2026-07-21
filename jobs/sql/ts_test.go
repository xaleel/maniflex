package sql

// Audit JB-7: SQLite compares the not_before / lease_until columns as TEXT, so
// their string form must sort the same way the instants do. time.RFC3339Nano
// omits trailing fractional zeros, giving variable-width strings that do not —
// within one second a whole-second stamp sorts after a fractional one, because
// the character after the seconds is 'Z' (0x5A) in one and '.' (0x2E) in the
// other. The fix pads the fraction to a fixed nine digits.
//
// SQLite's default BINARY collation compares byte-by-byte, exactly as Go's
// string comparison does for these ASCII strings, so these pure tests are a
// faithful stand-in for the database's ordering. The functional path is also
// covered end-to-end in tests/e2e (a sub-second delayed job).
//
//	go test ./jobs/sql/ -run TestTimestamp

import (
	"strings"
	"testing"
	"time"
)

// The property the SQL relies on: lexicographic order of ts() equals chronological
// order, including across the whole-second/fractional boundary that broke it.
func TestTimestamp_LexicographicOrderMatchesTime(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC)
	times := []time.Time{
		base.Add(-1 * time.Nanosecond),        // 55.999999999
		base,                                  // 56.000000000  (whole second)
		base.Add(1 * time.Nanosecond),         // 56.000000001
		base.Add(500 * time.Millisecond),      // 56.500000000
		base.Add(999999999 * time.Nanosecond), // 56.999999999
		base.Add(time.Second),                 // 57.000000000
	}
	for _, a := range times {
		for _, b := range times {
			lexLE := ts(a) <= ts(b)
			timeLE := !a.After(b)
			if lexLE != timeLE {
				t.Errorf("ts(%s)=%q vs ts(%s)=%q: lexicographic <= is %v but chronological <= is %v",
					a.Format(time.RFC3339Nano), ts(a),
					b.Format(time.RFC3339Nano), ts(b), lexLE, timeLE)
			}
		}
	}
}

// The exact failure the audit names: a job scheduled on a whole second must not
// sort as later than one scheduled a fraction into the same second.
func TestTimestamp_WholeSecondIsNotAfterFractionalSameSecond(t *testing.T) {
	whole := time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC)
	fractional := whole.Add(500 * time.Millisecond)

	if !(ts(whole) < ts(fractional)) {
		t.Errorf("ts(whole)=%q did not sort before ts(fractional)=%q: "+
			"a due job looks not-due (or a future job looks due) by up to a second",
			ts(whole), ts(fractional))
	}
}

// The fraction must be a fixed nine digits, which is what makes the width
// constant. A regression that reintroduced RFC3339Nano would drop the zeros.
func TestTimestamp_FractionIsFixedWidth(t *testing.T) {
	// A whole-second instant is where RFC3339Nano would omit the fraction entirely.
	got := ts(time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC))
	want := "2026-07-21T12:34:56.000000000Z"
	if got != want {
		t.Errorf("ts(whole second) = %q, want %q", got, want)
	}
	// Every stamp is the same length, so comparisons never straddle widths.
	if l := len(ts(time.Now())); l != len(want) {
		t.Errorf("ts() length = %d, want the fixed %d", l, len(want))
	}
}

// parseTS must round-trip the new format, and still read the variable-width
// RFC3339Nano rows written before this fix so an upgrade does not choke on them.
func TestTimestamp_ParseRoundTripsAndReadsLegacy(t *testing.T) {
	when := time.Date(2026, 7, 21, 12, 34, 56, 500000000, time.UTC)

	got := parseTS(ts(when))
	if got == nil || !got.Equal(when) {
		t.Errorf("round-trip: parseTS(ts(%v)) = %v", when, got)
	}

	// A legacy row: RFC3339Nano dropped the trailing zeros.
	legacy := when.Format(time.RFC3339Nano) // "2026-07-21T12:34:56.5Z"
	if strings.Contains(legacy, "000") {
		t.Fatalf("legacy fixture is not actually variable-width: %q", legacy)
	}
	if got := parseTS(legacy); got == nil || !got.Equal(when) {
		t.Errorf("legacy parse: parseTS(%q) = %v, want %v", legacy, got, when)
	}
}
