package maniflex

import (
	"errors"
	"testing"
)

// Range-header resolution behind 3B.3b has three distinct outcomes that must
// not be confused: a resolved window (206), "not usable, serve the whole
// object" (200), and unsatisfiable (416). One test each.

// rangeTestSize is the object length every case below resolves against.
const rangeTestSize = 1000

func TestParseByteRange_ResolvesWindows(t *testing.T) {
	cases := []struct {
		name             string
		hdr              string
		start, length    int64
		wantContentRange string
	}{
		{"closed", "bytes=0-499", 0, 500, "bytes 0-499/1000"},
		{"closed_midway", "bytes=200-299", 200, 100, "bytes 200-299/1000"},
		{"open_ended", "bytes=900-", 900, 100, "bytes 900-999/1000"},
		{"suffix", "bytes=-100", 900, 100, "bytes 900-999/1000"},
		{"single_byte", "bytes=0-0", 0, 1, "bytes 0-0/1000"},
		{"last_byte", "bytes=999-", 999, 1, "bytes 999-999/1000"},
		// An end past the last byte is clamped, not refused.
		{"end_past_eof_clamps", "bytes=990-5000", 990, 10, "bytes 990-999/1000"},
		// A suffix longer than the object is the whole object.
		{"suffix_past_eof_clamps", "bytes=-5000", 0, 1000, "bytes 0-999/1000"},
		{"whitespace_tolerated", " bytes= 10 - 19 ", 10, 10, "bytes 10-19/1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br, ok, err := parseByteRange(tc.hdr, rangeTestSize)
			if err != nil || !ok {
				t.Fatalf("parseByteRange(%q) = ok:%v err:%v, want a resolved window", tc.hdr, ok, err)
			}
			if br.start != tc.start || br.length != tc.length {
				t.Errorf("parseByteRange(%q) = start:%d len:%d, want start:%d len:%d",
					tc.hdr, br.start, br.length, tc.start, tc.length)
			}
			if got := br.contentRange(); got != tc.wantContentRange {
				t.Errorf("contentRange() = %q, want %q", got, tc.wantContentRange)
			}
		})
	}
}

// Everything a server may legally ignore, answering 200 with the whole object.
func TestParseByteRange_DeclinesToWholeObject(t *testing.T) {
	for _, hdr := range []string{
		"",                // no Range at all
		"bytes=",          // empty spec
		"bytes=abc-def",   // not numbers
		"items=0-10",      // unsupported unit
		"0-10",            // missing unit
		"bytes=0-499,-50", // multi-range: would need multipart/byteranges
		"bytes=500-200",   // end before start
		"bytes=-abc",      // unparseable suffix
		"bytes=10",        // no dash
	} {
		br, ok, err := parseByteRange(hdr, rangeTestSize)
		if ok || err != nil {
			t.Errorf("parseByteRange(%q) = %+v ok:%v err:%v, want declined (serve 200)",
				hdr, br, ok, err)
		}
	}
}

func TestParseByteRange_UnsatisfiableIs416(t *testing.T) {
	for _, hdr := range []string{
		"bytes=1000-",     // starts exactly at EOF
		"bytes=5000-6000", // starts well past EOF
		"bytes=-0",        // "the last zero bytes"
	} {
		_, ok, err := parseByteRange(hdr, rangeTestSize)
		if ok || !errors.Is(err, errRangeUnsatisfiable) {
			t.Errorf("parseByteRange(%q) = ok:%v err:%v, want errRangeUnsatisfiable", hdr, ok, err)
		}
	}
}

// A backend that does not report a size cannot have a range resolved against
// it — serve the whole object rather than guess at the bounds.
func TestParseByteRange_UnknownSizeDeclines(t *testing.T) {
	for _, size := range []int64{0, -1} {
		if _, ok, err := parseByteRange("bytes=0-99", size); ok || err != nil {
			t.Errorf("parseByteRange with size %d = ok:%v err:%v, want declined", size, ok, err)
		}
	}
}
