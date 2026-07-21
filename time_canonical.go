package maniflex

import (
	"reflect"
	"strings"
	"time"
)

// CanonicalTimeLayout is the string form maniflex writes time.Time values in for
// the SQL adapters, and the form it canonicalises timestamp filter values into.
//
// SQLite has no native timestamp type: the adapters store timestamps as TEXT and
// compare them with the default BINARY collation, i.e. byte-by-byte. For a range
// filter (created_at >= ?, a scheduled due-check, a cursor) to order rows the way
// the instants order, the string form must sort lexicographically the same way it
// sorts in time. time.RFC3339Nano does not: it drops trailing fractional zeros, so
// its width varies, and within one second a whole-second stamp ("…56Z") sorts
// AFTER a fractional one ("…56.5Z") because the byte after the seconds is 'Z'
// (0x5A) in one and '.' (0x2E) in the other. This layout pads the fraction to a
// fixed nine digits so every stamp is the same width and byte order tracks time
// order. (jobs/sql fixes the identical bug behind its own ts() — audit JB-7.)
//
// Postgres stores TIMESTAMPTZ and compares natively, so it is unaffected either
// way; feeding it this form is harmless because it is valid RFC3339.
const CanonicalTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// CanonicalTime formats an instant in CanonicalTimeLayout, in UTC.
func CanonicalTime(t time.Time) string {
	return t.UTC().Format(CanonicalTimeLayout)
}

// canonicalTimeValue re-formats a single filter value that is a full RFC3339
// timestamp into CanonicalTimeLayout (in UTC), returning ok=false for anything
// that is not a full timestamp — a date-only value like "2026-07-21", or a
// non-time string — which the caller then leaves exactly as the client sent it.
//
// This is what lets ?filter=created_at:gte:<ts> order correctly on SQLite without
// changing the meaning of a date-only bound or rejecting input the adapter would
// otherwise have accepted. A value carrying a zone offset is normalised to UTC so
// it compares against the UTC strings the write path stores.
func canonicalTimeValue(s string) (string, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return CanonicalTime(t), true
		}
	}
	return "", false
}

// canonicalizeTimeFilterValue canonicalises a filter value bound against a
// time-typed column. Scalar operators carry one value; between/in/not_in carry a
// comma-separated list, each element of which is canonicalised independently.
// Elements that are not full timestamps pass through unchanged, so date-only
// bounds and empty entries keep their existing behaviour.
func canonicalizeTimeFilterValue(op FilterOperator, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch op {
	case OpBetween, OpIn, OpNotIn:
		parts := SplitCSV(s)
		for i, p := range parts {
			if c, ok := canonicalTimeValue(p); ok {
				parts[i] = c
			}
		}
		return strings.Join(parts, ",")
	default:
		if c, ok := canonicalTimeValue(s); ok {
			return c
		}
		return s
	}
}

// isTimeType reports whether t is time.Time or *time.Time — the Go types the
// adapters store through the timestamp path canonicalTimeValue matches.
func isTimeType(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == reflect.TypeOf(time.Time{})
}
