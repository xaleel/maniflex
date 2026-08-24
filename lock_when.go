package maniflex

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// LockCondition expresses a single mfx:"lock_when:<field>=<value>" directive.
// When a record's current state matches every Field/Value pair across all
// conditions attached to the model, the record is "locked" — updates and
// deletes return 422 RECORD_LOCKED. Each LockCondition contributes one
// independent rule; if any rule matches, the record is locked.
type LockCondition struct {
	// JSONName is the JSON field name the condition compares against (the
	// directive is declared as `lock_when:status=posted`; JSONName="status").
	// At registration this is resolved against the model's JSON names so a
	// typo is caught early rather than silently never matching.
	JSONName string

	// Value is the right-hand side of the equality check, as written in the tag.
	// Kept verbatim for the RECORD_LOCKED message; the comparison uses the
	// canonical form below.
	Value string

	// canonical is Value rendered in the one form the field's type has, and kind
	// is that type's kind. collectLockWhen fills both once the directive is
	// resolved against a real field.
	//
	// The comparison used to be fmt.Sprintf("%v", recordValue) == Value, which
	// made it a question about formatting rather than about equality. %v renders
	// a float64 as %g does, so a column holding 1000000 stringified to "1e+06"
	// and a tag saying 1000000 never fired (audit O8) — and the failure is open:
	// the record is left unlocked, which is the wrong direction for a rule whose
	// only job is to refuse writes.
	//
	// A zero kind means the condition was never resolved against a field, which
	// only happens for a LockCondition built by hand rather than by the parser.
	// Those keep the original string comparison.
	canonical string
	kind      reflect.Kind
}

// parseLockWhen parses a `lock_when:field=value` directive. Returns ok=false
// when the directive is malformed (missing `=` or empty field/value).
func parseLockWhen(part string) (LockCondition, bool) {
	rest := strings.TrimPrefix(part, "lock_when:")
	eq := strings.IndexByte(rest, '=')
	if eq <= 0 || eq == len(rest)-1 {
		return LockCondition{}, false
	}
	return LockCondition{
		JSONName: strings.TrimSpace(rest[:eq]),
		Value:    strings.TrimSpace(rest[eq+1:]),
	}, true
}

// matchesRecord reports whether the loaded record state satisfies this
// condition. The record map keys are JSON names (as produced by toJSONMap).
//
// Both sides are reduced to the canonical form of the field's own type before
// comparing, so the verdict does not depend on which shape a driver happened to
// hand back: SQLite reports a bool column as an integer, Postgres as a bool, and
// a NUMERIC arrives through lib/pq as text. toJSONMap already smooths most of
// that over, but a rule that refuses writes should not be relying on it.
func (lc LockCondition) matchesRecord(record map[string]any) bool {
	v, ok := record[lc.JSONName]
	if !ok || v == nil {
		return false
	}
	if lc.kind == reflect.Invalid {
		return fmt.Sprintf("%v", v) == lc.Value // hand-built condition
	}
	got, ok := canonicalLockValue(lc.kind, v)
	return ok && got == lc.canonical
}

// canonicalLockValue renders v in the single form kind has, so two values that
// are equal produce equal strings whatever their Go type. It reports false when
// v cannot be read as that kind at all, which is a non-match rather than an
// error: the record simply does not hold the value the rule names.
func canonicalLockValue(kind reflect.Kind, v any) (string, bool) {
	switch kind {
	case reflect.Bool:
		switch t := v.(type) {
		case bool:
			return strconv.FormatBool(t), true
		case string:
			b, err := strconv.ParseBool(t)
			return strconv.FormatBool(b), err == nil
		}
		if f, ok := lockNumeric(v); ok {
			return strconv.FormatBool(f != 0), true
		}
		return "", false

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		f, ok := lockNumeric(v)
		if !ok {
			return "", false
		}
		// What fixes the 1e+06 mismatch is that the tag and the record now reduce
		// through this same function; any consistent rendering would do. 'f' with
		// precision -1 is chosen for legibility if the value is ever surfaced —
		// it keeps an integer an integer and a fraction a fraction — and it makes
		// an int column reporting 5 as int64, float64 or "5" one string.
		return strconv.FormatFloat(f, 'f', -1, 64), true

	case reflect.String:
		if s, ok := v.(string); ok {
			return s, true
		}
		return fmt.Sprintf("%v", v), true
	}
	return "", false
}

// lockNumeric reads v as a float64 whatever numeric shape it arrived in,
// including the decimal text lib/pq returns for NUMERIC.
func lockNumeric(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// collectLockWhen flattens per-field LockWhen tag lists onto m.LockWhen and
// validates that every referenced JSON name resolves to a real field on the
// model. A typo like `lock_when:satus=posted` would otherwise produce a rule
// that never matches; surface it at registration instead.
// It also resolves each directive's value against that field's type. Only the
// field name used to be checked, so a value the column could never hold —
// lock_when:n=five on an integer, or a date against a time.Time — registered
// happily and produced a rule that could not fire. A lock that never fires reads
// exactly like a record that is not locked (audit O8).
func (m *ModelMeta) collectLockWhen() error {
	for _, f := range m.Fields {
		for _, lc := range f.Tags.LockWhen {
			target := m.FieldByJSONName(lc.JSONName)
			if target == nil {
				return fmt.Errorf(
					"maniflex: model %q lock_when references unknown json field %q",
					m.Name, lc.JSONName)
			}
			t := target.Type
			if t != nil && t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t == nil || !lockComparableKind(t.Kind()) {
				return fmt.Errorf(
					"maniflex: model %q lock_when:%s=%s compares against a %s field; "+
						"the directive tests equality and supports string, bool and numeric "+
						"columns only",
					m.Name, lc.JSONName, lc.Value, target.Type)
			}
			canonical, ok := canonicalLockValue(t.Kind(), lc.Value)
			if !ok {
				return fmt.Errorf(
					"maniflex: model %q lock_when:%s=%s — %q is not a valid %s, so the "+
						"condition could never match and the record would never lock",
					m.Name, lc.JSONName, lc.Value, lc.Value, t)
			}
			lc.canonical = canonical
			lc.kind = t.Kind()
			m.LockWhen = append(m.LockWhen, lc)
		}
	}
	return nil
}

// lockComparableKind reports whether equality against a tag's text means
// anything for this kind. A time.Time or a struct is excluded: its %v form is
// not something anyone would write in a tag, so such a directive is a mistake
// rather than a rule.
func lockComparableKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
