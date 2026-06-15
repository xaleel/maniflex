package maniflex

import (
	"fmt"
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

	// Value is the right-hand side of the equality check. Stored as a string
	// because the directive itself is textual; numeric/boolean comparisons go
	// through fmt.Sprintf("%v", ...) on the loaded record value to flatten
	// type differences.
	Value string
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
func (lc LockCondition) matchesRecord(record map[string]any) bool {
	v, ok := record[lc.JSONName]
	if !ok || v == nil {
		return false
	}
	return fmt.Sprintf("%v", v) == lc.Value
}

// collectLockWhen flattens per-field LockWhen tag lists onto m.LockWhen and
// validates that every referenced JSON name resolves to a real field on the
// model. A typo like `lock_when:satus=posted` would otherwise produce a rule
// that never matches; surface it at registration instead.
func (m *ModelMeta) collectLockWhen() error {
	for _, f := range m.Fields {
		for _, lc := range f.Tags.LockWhen {
			if m.FieldByJSONName(lc.JSONName) == nil {
				return fmt.Errorf(
					"maniflex: model %q lock_when references unknown json field %q",
					m.Name, lc.JSONName)
			}
			m.LockWhen = append(m.LockWhen, lc)
		}
	}
	return nil
}
