package maniflex

import (
	"reflect"
	"strings"
	"time"
)

// ResolveFilterField resolves a FilterExpr.Field to the model field it names,
// accepting either the DB column name or the json name — the same two spellings
// ParseFilterParam accepts from a URL. It returns nil when neither matches.
//
// The two spellings exist because a FilterExpr can arrive two ways. One is
// parsed from a URL, where resolveFlatFilter has always tried the json name
// first and rewritten Field to the DB name before the adapter sees it. The
// other is built in Go — by a tenancy middleware, a hand-written action, or the
// framework itself — and reaches the adapter exactly as it was typed. A caller
// writing that second kind naturally reaches for the name the model publishes,
// which is the json one; it then became a column no table has, and the request
// failed as an unknown-column SQL error rather than anything nameable.
func (m *ModelMeta) ResolveFilterField(name string) *FieldMeta {
	if f := m.FieldByDBName(name); f != nil {
		return f
	}
	return m.FieldByJSONName(name)
}

// NormalizeFilterValue coerces one filter value into the form the target column
// actually binds against, and is the read-path counterpart to the write path's
// normalise: a value written through Create and the same value used to filter
// for the row it wrote must reach the driver in the same shape.
//
// f may be nil (an unresolved field), in which case the value is returned
// unchanged — normalisation has nothing to measure it against, and the caller
// decides what an unknown field means.
//
// Two coercions matter, both of which used to be skipped for a filter that did
// not come from a URL:
//
//   - time.Time and *time.Time are formatted in CanonicalTimeLayout. SQLite
//     stores timestamps as TEXT and compares them byte-by-byte, so a driver's
//     own rendering of an instant is a different string than the one in the
//     column and compares against a value not in the table. Strings keep the
//     existing behaviour: a full RFC3339 stamp is re-emitted canonically, and a
//     date-only bound is left exactly as written.
//
//   - A bool column takes the boolean spellings ("true"/"false", "1"/"0") as a
//     real Go bool. A bound string is not a SQL literal: SQLite stores bools in
//     an INTEGER column, and while '0' converts under numeric affinity, 'false'
//     does not look numeric, stays TEXT, and sorts after every INTEGER — so it
//     can never compare equal and the list comes back empty with nothing
//     logged. Postgres casts either spelling happily, which is what made this
//     pass a Postgres CI lane and fail on a SQLite dev box.
//
// Anything else is returned unchanged. A value that is not a recognised
// spelling for its column's kind is left alone rather than guessed at, so
// normalisation can only ever put a value into the form the caller already
// meant — it never invents a predicate they did not write.
func NormalizeFilterValue(f *FieldMeta, op FilterOperator, v any) any {
	if f == nil || v == nil {
		return v
	}
	switch op {
	// These bind no value at all.
	case OpIsNull, OpNotNull:
		return v
	// These run against a LIKE pattern built from the value, so coercing it to a
	// bool or a timestamp would destroy the pattern.
	case OpLike, OpILike, OpContains, OpStartsWith, OpEndsWith:
		return v
	}

	switch {
	case isTimeType(f.Type):
		return normalizeTimeFilterValue(op, v)
	case isBoolType(f.Type):
		return normalizeBoolFilterValue(op, v)
	}
	return v
}

// isBoolType reports whether t is bool or *bool.
func isBoolType(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Bool
}

// normalizeTimeFilterValue extends canonicalizeTimeFilterValue — which handles
// the string forms a URL carries — to the real time.Time a Go-built filter
// carries.
func normalizeTimeFilterValue(op FilterOperator, v any) any {
	if t, ok := derefTime(v); ok {
		return CanonicalTime(t)
	}
	return canonicalizeTimeFilterValue(op, v)
}

// derefTime unwraps a time.Time or a non-nil *time.Time.
func derefTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	}
	return time.Time{}, false
}

// normalizeBoolFilterValue coerces the boolean spellings to a real bool. The set
// operators carry a comma-separated list whose elements the adapters split with
// SplitCSV, so each element is coerced on its own and the value stays a joined
// string — the shape inCond and betweenCond expect.
func normalizeBoolFilterValue(op FilterOperator, v any) any {
	switch op {
	case OpIn, OpNotIn, OpBetween:
		s, ok := v.(string)
		if !ok {
			return v
		}
		parts := SplitCSV(s)
		for i, p := range parts {
			if b, ok := parseBoolWord(p); ok {
				if b {
					parts[i] = "1"
				} else {
					parts[i] = "0"
				}
			}
		}
		return strings.Join(parts, ",")
	}

	s, ok := v.(string)
	if !ok {
		return v
	}
	if b, ok := parseBoolWord(s); ok {
		return b
	}
	return v
}

// parseBoolWord accepts the spellings a client plausibly sends for a boolean —
// the JSON/Go words and the 1/0 the column stores — case-insensitively. It
// deliberately does not accept strconv.ParseBool's wider set ("t", "F"), which
// are likelier to be a typo than an intent.
func parseBoolWord(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}
