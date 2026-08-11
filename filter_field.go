package maniflex

import (
	"fmt"
	"reflect"
	"strings"
)

// filterTypeClass groups a column's Go type into the set of columns it may be
// compared against with a *_field operator. Two columns compare only when their
// classes match.
//
// The check is not fussiness — it is the only place the two drivers can be made
// to agree. Postgres refuses "bigint >= text" outright; SQLite applies storage
// class ordering and answers, with INTEGER sorting before TEXT, so every numeric
// row compares less than every string one and the filter returns a confident,
// wrong list. Left to the database, the same request 400s in production and
// quietly lies on a developer's SQLite box. This is the same divergence class as
// the bool-bound-as-TEXT filter bug and the NULLIF wrap on Div, and the same
// answer: settle it before the SQL is built.
type filterTypeClass string

const (
	classNone   filterTypeClass = ""
	classNumber filterTypeClass = "number"
	classString filterTypeClass = "string"
	classBool   filterTypeClass = "bool"
	classTime   filterTypeClass = "time"
)

// fieldTypeClass reports the comparability class of a field's Go type, or
// classNone for a type with no ordering a WHERE clause can use — a slice, a map,
// a locale document, a struct that is not a timestamp.
func fieldTypeClass(t reflect.Type) filterTypeClass {
	if t == nil {
		return classNone
	}
	// time.Time is a struct, so it must be settled before the Kind switch below
	// or it would fall through to classNone. isTimeType handles the pointer.
	if isTimeType(t) {
		return classTime
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return classBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return classNumber
	case reflect.String:
		return classString
	}
	return classNone
}

// resolveFieldComparison resolves both sides of a *_field filter, rejecting
// anything the renderer would have to fail closed on — so a client gets a 400
// naming the problem rather than an empty list that looks like no matching data.
//
// Cross-table comparison is out of scope. A nested right-hand side would need
// the caller's FROM to have joined that relation, which ctx.Aggregate cannot do
// (see BuildFilterSQL's note on nested filters), so the same filter would list
// rows and then fail to aggregate them — the divergence AG-5 exists to prevent.
// Both sides are therefore refused on a dot, including a locale sub-key.
func resolveFieldComparison(expr *FilterExpr, leftName, rightName string, model *ModelMeta) (*FilterExpr, error) {
	rightName = strings.TrimSpace(rightName)
	if strings.Contains(leftName, ".") || strings.Contains(rightName, ".") {
		return nil, fmt.Errorf(
			"operator %q compares two columns of model %s; a relation or locale field is not supported (got %q and %q)",
			expr.Operator, model.Name, leftName, rightName)
	}

	left, err := comparableField(leftName, model)
	if err != nil {
		return nil, err
	}
	right, err := comparableField(rightName, model)
	if err != nil {
		return nil, err
	}

	lc, rc := fieldTypeClass(left.Type), fieldTypeClass(right.Type)
	if lc == classNone || rc == classNone {
		return nil, fmt.Errorf(
			"operator %q needs two comparable columns, but %q or %q holds a value with no ordering "+
				"(only numbers, strings, booleans and timestamps compare)",
			expr.Operator, leftName, rightName)
	}
	if lc != rc {
		return nil, fmt.Errorf(
			"operator %q compares %q (%s) with %q (%s); both columns must hold the same kind of value",
			expr.Operator, leftName, lc, rightName, rc)
	}

	expr.Field = left.Tags.DBName
	expr.ValueField = right.Tags.DBName
	// Value carried the right-hand field name through parsing. Clearing it is
	// what keeps a column name from ever reaching a placeholder binder.
	expr.Value = nil
	return expr, nil
}

// comparableField resolves one side of a *_field filter to a field a client is
// allowed to compare on. The gates are the ones an ordinary filter's left side
// already passes — the right side is no less client-controlled, so it earns no
// weaker check.
func comparableField(name string, model *ModelMeta) (*FieldMeta, error) {
	f := model.ResolveFilterField(name)
	if f == nil {
		return nil, fmt.Errorf("field %q not found on model %s", name, model.Name)
	}
	if f.Tags.Encrypted {
		return nil, fmt.Errorf(
			"filtering on encrypted field %q is not supported (ENCRYPTED_FIELD_NOT_FILTERABLE)", name)
	}
	if !f.Tags.Filterable {
		return nil, fmt.Errorf("field %q on model %s is not filterable (%s)",
			name, model.Name, howToAllow(f.Tags.DBName, "filterable"))
	}
	return f, nil
}
