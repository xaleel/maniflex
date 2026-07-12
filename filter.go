package maniflex

import (
	"fmt"
	"strings"
)

// FilterOperator is a comparison operator used in filter expressions.
type FilterOperator string

const (
	OpEq      FilterOperator = "eq"       // field = value
	OpNeq     FilterOperator = "neq"      // field != value
	OpGt      FilterOperator = "gt"       // field > value
	OpGte     FilterOperator = "gte"      // field >= value
	OpLt      FilterOperator = "lt"       // field < value
	OpLte     FilterOperator = "lte"      // field <= value
	OpLike    FilterOperator = "like"     // field LIKE value (case-sensitive)
	OpILike   FilterOperator = "ilike"    // field ILIKE value (case-insensitive)
	OpIn      FilterOperator = "in"       // field IN (v1,v2,...)
	OpNotIn   FilterOperator = "not_in"   // field NOT IN (v1,v2,...)
	OpIsNull  FilterOperator = "is_null"  // field IS NULL  (no value)
	OpNotNull FilterOperator = "not_null" // field IS NOT NULL (no value)
	OpBetween FilterOperator = "between"  // field BETWEEN lo AND hi (value "lo,hi")
)

var validOperators = map[FilterOperator]bool{
	OpEq: true, OpNeq: true, OpGt: true, OpGte: true,
	OpLt: true, OpLte: true, OpLike: true, OpILike: true,
	OpIn: true, OpNotIn: true, OpIsNull: true, OpNotNull: true,
	OpBetween: true,
}

// FilterExpr is a single parsed and validated filter condition.
type FilterExpr struct {
	// Flat filter (not nested)
	Field    string         // DB column name on the primary table
	Operator FilterOperator
	Value    any // raw string from URL; adapters cast as needed

	// Nested filter (Field contains a "." and references a BelongsTo relation)
	IsNested      bool
	RelationKey   string // e.g. "author"
	RelationModel string // e.g. "Author"
	RelationTable string // DB table of related model, e.g. "users"
	RelationFK    string // FK column on THIS table, e.g. "author_id"
	NestedField   string // DB column on the related table, e.g. "status"

	// Locale filter (Field contains a "." and the left side is a locale field)
	// e.g. ?filter=name.ar:ilike:قلب
	// SQL: name->>'ar' ILIKE ? (Postgres) / json_extract(name,'$.ar') LIKE ? (SQLite)
	IsLocale  bool   // true when filtering on a locale sub-key
	LocaleKey string // the locale key portion, e.g. "ar"

	// Group is the OR-group index.
	//
	// Group <= 0 (which includes the struct's zero value) means ungrouped: the
	// filter forms its own AND clause. Filters sharing the same Group >= 1 are
	// OR-ed together. The zero value is ungrouped on purpose so a hand-built
	// FilterExpr AND-s by default — building one with the bare Field/Operator/
	// Value set is the common case and must not silently OR.
	//
	// The URL bracket syntax ?filter[N]=... (N >= 0) is mapped onto Group N+1
	// during parsing, so e.g. ?filter[0]=a&filter[0]=b is one OR group; the
	// external contract is unchanged.
	Group int
}

// validateFilterGroups checks that all filters within the same OR group target
// the same table. Cross-table OR is not supported and returns a 400 error.
func validateFilterGroups(filters []*FilterExpr, primaryTable string) error {
	// map[group] -> first table seen for that group
	groupTable := make(map[int]string)
	for _, f := range filters {
		if f.Group <= 0 {
			continue
		}
		table := primaryTable
		if f.IsNested {
			table = f.RelationTable
		}
		if prev, ok := groupTable[f.Group]; ok {
			if prev != table {
				return fmt.Errorf("OR filter groups must target the same table (group %d mixes %q and %q)", f.Group, prev, table)
			}
		} else {
			groupTable[f.Group] = table
		}
	}
	return nil
}

// ParseFilterParam parses one filter query parameter value into a FilterExpr.
//
// Format:  field:operator[:value]
//
// Examples:
//
//	status:eq:published
//	created_at:gte:2024-01-01
//	author.status:neq:banned      (nested — author must be a registered relation)
//	deleted_at:is_null            (no value)
//	role:in:admin,editor
func ParseFilterParam(raw string, model *ModelMeta, reg RegistryAccessor) (*FilterExpr, error) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid filter %q: expected field:operator[:value]", raw)
	}

	fieldPath := parts[0]
	op := FilterOperator(parts[1])
	var value any
	if len(parts) == 3 {
		value = parts[2]
	}

	if !validOperators[op] {
		return nil, fmt.Errorf("unknown filter operator %q", op)
	}

	if op == OpBetween {
		if value == nil || len(SplitCSV(fmt.Sprint(value))) != 2 {
			return nil, fmt.Errorf("operator %q requires two comma-separated values (e.g. amount:between:100,500)", op)
		}
	}

	// An empty list has no meaningful SQL form: "role:in:" (and "role:in:,,",
	// whose entries all drop out) would otherwise reach the adapter as zero
	// values and emit "role IN ()" — a syntax error on every driver, so a client
	// could provoke a 500 at will (BUG-7).
	if op == OpIn || op == OpNotIn {
		if value == nil || len(SplitCSV(fmt.Sprint(value))) == 0 {
			return nil, fmt.Errorf("operator %q requires at least one comma-separated value (e.g. role:in:admin,editor)", op)
		}
	}

	expr := &FilterExpr{Operator: op, Value: value, Group: -1}

	if strings.Contains(fieldPath, ".") {
		return resolveNestedFilter(expr, fieldPath, model, reg)
	}
	return resolveFlatFilter(expr, fieldPath, model)
}

func resolveFlatFilter(expr *FilterExpr, fieldPath string, model *ModelMeta) (*FilterExpr, error) {
	f := model.FieldByJSONName(fieldPath)
	if f == nil {
		// Also try by DB name
		f = model.FieldByDBName(fieldPath)
	}
	if f == nil {
		return nil, fmt.Errorf("field %q not found on model %s", fieldPath, model.Name)
	}
	if f.Tags.Encrypted {
		return nil, fmt.Errorf("filtering on encrypted field %q is not supported (ENCRYPTED_FIELD_NOT_FILTERABLE)", fieldPath)
	}
	if !f.Tags.Filterable {
		return nil, fmt.Errorf("field %q on model %s is not filterable (add mfx:\"filterable\" to the struct tag)", fieldPath, model.Name)
	}
	expr.Field = f.Tags.DBName
	return expr, nil
}

func resolveNestedFilter(expr *FilterExpr, fieldPath string, model *ModelMeta, reg RegistryAccessor) (*FilterExpr, error) {
	dot := strings.SplitN(fieldPath, ".", 2)
	relKey := dot[0]
	nestedField := dot[1]

	// Check if the left side is a locale field before looking for a relation.
	if f := model.FieldByJSONName(relKey); f != nil && f.Tags.Locale {
		if !f.Tags.Filterable {
			return nil, fmt.Errorf("field %q on model %s is not filterable (add mfx:\"filterable\" to the struct tag)", relKey, model.Name)
		}
		// The locale sub-key is targeted into a JSON-path expression by the query
		// builder, so it is held to a strict allowlist and rejected here rather
		// than reaching the SQL layer (SEC-1: SQL injection via the filter key).
		if !isLocaleKey(nestedField) {
			return nil, fmt.Errorf("invalid locale key %q in filter %q: must be a locale identifier ([A-Za-z_][A-Za-z0-9_-]*)", nestedField, fieldPath)
		}
		expr.Field = f.Tags.DBName
		expr.IsLocale = true
		expr.LocaleKey = nestedField
		return expr, nil
	}

	rel := model.RelationByKey(relKey)
	if rel == nil {
		return nil, fmt.Errorf("relation %q not found on model %s", relKey, model.Name)
	}
	if rel.Kind != BelongsTo {
		return nil, fmt.Errorf("nested filters are only supported on BelongsTo relations (got HasMany for %q)", relKey)
	}

	relMeta, ok := reg.Get(rel.RelatedModel)
	if !ok {
		return nil, fmt.Errorf("related model %q is not registered", rel.RelatedModel)
	}

	nf := relMeta.FieldByJSONName(nestedField)
	if nf == nil {
		nf = relMeta.FieldByDBName(nestedField)
	}
	if nf == nil {
		return nil, fmt.Errorf("field %q not found on related model %s", nestedField, relMeta.Name)
	}
	if !nf.Tags.Filterable {
		return nil, fmt.Errorf("field %q on related model %s is not filterable", nestedField, relMeta.Name)
	}

	expr.Field = fieldPath
	expr.IsNested = true
	expr.RelationKey = relKey
	expr.RelationModel = rel.RelatedModel
	expr.RelationTable = relMeta.TableName
	expr.RelationFK = rel.FKColumn
	expr.NestedField = nf.Tags.DBName

	return expr, nil
}

// resolveNestedSort resolves a "relation.field" sort name into a nested SortExpr.
// Only BelongsTo relations are supported (the same constraint as nested filters);
// the related field must be marked sortable. The query builder adds a LEFT JOIN
// on the relation and orders by the related table's column.
func resolveNestedSort(fieldPath string, dir SortDir, model *ModelMeta, reg RegistryAccessor) (SortExpr, error) {
	dot := strings.SplitN(fieldPath, ".", 2)
	relKey := dot[0]
	nestedField := dot[1]

	rel := model.RelationByKey(relKey)
	if rel == nil {
		return SortExpr{}, fmt.Errorf("sort field %q not found on model %s", fieldPath, model.Name)
	}
	if rel.Kind != BelongsTo {
		return SortExpr{}, fmt.Errorf("nested sorts are only supported on BelongsTo relations (got HasMany for %q)", relKey)
	}

	relMeta, ok := reg.Get(rel.RelatedModel)
	if !ok {
		return SortExpr{}, fmt.Errorf("related model %q is not registered", rel.RelatedModel)
	}

	nf := relMeta.FieldByJSONName(nestedField)
	if nf == nil {
		nf = relMeta.FieldByDBName(nestedField)
	}
	if nf == nil {
		return SortExpr{}, fmt.Errorf("field %q not found on related model %s", nestedField, relMeta.Name)
	}
	if !nf.Tags.Sortable {
		return SortExpr{}, fmt.Errorf("field %q on related model %s is not sortable", nestedField, relMeta.Name)
	}

	return SortExpr{
		DBName:        nf.Tags.DBName,
		Direction:     dir,
		IsNested:      true,
		RelationKey:   relKey,
		RelationModel: rel.RelatedModel,
		RelationTable: relMeta.TableName,
		RelationFK:    rel.FKColumn,
		NestedField:   nf.Tags.DBName,
	}, nil
}

// isLocaleKey reports whether s is a valid locale sub-key — a BCP-47-style
// language tag made of letters, digits, '-' and '_', starting with a letter or
// underscore (e.g. "en", "ar", "en-US", "zh_Hans").
//
// Locale keys are targeted into a JSON-path expression by the query builder
// (name->>'<key>' on Postgres, json_extract(name,'$.<key>') on SQLite). The
// builder binds the key as a parameter, but this allowlist is the primary,
// defence-in-depth guard: it rejects an injection payload at parse time with a
// clear 400 rather than letting it reach the SQL layer (SEC-1). The same
// allowlist is intended to gate the locale sort path.
func isLocaleKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if i == 0 {
			if !letter {
				return false
			}
			continue
		}
		if !letter && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return true
}
