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
	OpLike    FilterOperator = "like"     // field LIKE value — value is a raw pattern (case-sensitive)
	OpILike   FilterOperator = "ilike"    // field ILIKE value — value is a raw pattern (case-insensitive)
	OpIn      FilterOperator = "in"       // field IN (v1,v2,...)
	OpNotIn   FilterOperator = "not_in"   // field NOT IN (v1,v2,...)
	OpIsNull  FilterOperator = "is_null"  // field IS NULL  (no value)
	OpNotNull FilterOperator = "not_null" // field IS NOT NULL (no value)
	OpBetween FilterOperator = "between"  // field BETWEEN lo AND hi (value "lo,hi")

	// The substring operators below take a literal value, not a pattern: % and _
	// in it are escaped and match themselves. Use them for user-typed text (a
	// search box, a filename); reach for like/ilike only when the caller really
	// is writing a pattern. All three are case-insensitive.
	OpContains   FilterOperator = "contains"    // field contains value  (%value%)
	OpStartsWith FilterOperator = "starts_with" // field starts with value (value%)
	OpEndsWith   FilterOperator = "ends_with"   // field ends with value   (%value)
)

var validOperators = map[FilterOperator]bool{
	OpEq: true, OpNeq: true, OpGt: true, OpGte: true,
	OpLt: true, OpLte: true, OpLike: true, OpILike: true,
	OpIn: true, OpNotIn: true, OpIsNull: true, OpNotNull: true,
	OpBetween: true,
	OpContains: true, OpStartsWith: true, OpEndsWith: true,
}

// Valid reports whether o is an operator the query builder implements.
//
// ParseFilterParam checks this for every filter arriving over HTTP, so a client
// cannot get an unknown operator past it. A FilterExpr built in Go bypasses that
// parse entirely, and Operator is a bare string type — which is what makes a
// typo (Operator: "equals", where the constant OpEq is "eq") compile, register,
// and reach the adapter as a predicate nothing recognises. Valid is exported so
// a custom adapter can hold the same line the shipped one does.
func (o FilterOperator) Valid() bool { return validOperators[o] }

// validateFilterOperators rejects a filter whose operator no adapter implements.
//
// It exists because such a filter used to be ignored rather than refused: the
// query builder's switch fell through to a constant-true predicate, so the
// condition vanished from the WHERE clause and the query returned every row.
// That is merely wrong for a client's filter and dangerous for a forced one —
// db.Tenancy's scope with a misspelt operator is not a narrower scope, it is no
// scope, on reads and (since the write path reads back through the same filters)
// on writes too. The failure was silent in both directions: nothing logged, and
// the extra rows look exactly like data.
//
// The adapters degrade an unrecognised operator to a false predicate as a
// backstop, so a filter reaching one by some path this check does not cover
// matches nothing rather than everything. This is the layer that can say why.
func validateFilterOperators(fs []*FilterExpr) error {
	for _, f := range fs {
		if f == nil || f.Operator.Valid() {
			continue
		}
		return fmt.Errorf(
			"maniflex: filter on %q uses unknown operator %q — a FilterExpr built in Go is not "+
				"parsed, so the operator is whatever was typed; use one of the maniflex.Op* "+
				"constants (OpEq, OpIn, OpBetween, …)",
			f.Field, f.Operator)
	}
	return nil
}

// LikeEscapeChar is the escape character in the patterns LikePattern builds. Every
// LIKE/ILIKE that consumes such a pattern must spell out ESCAPE '\': SQLite has no
// escape character by default and Postgres has a backslash, so saying it out loud
// is what makes the two agree.
const LikeEscapeChar = `\`

// LikePattern turns a user-supplied value into the LIKE pattern for one of the
// substring operators (contains, starts_with, ends_with). The LIKE
// metacharacters in the value — % and _, and the escape character itself — are
// escaped so they match literally, and the wildcards the operator implies are
// added around the result. Filtering for "50%" therefore finds the literal "50%"
// rather than everything beginning with 50.
//
// Returns "" for any other operator. Exported so DB adapters in sub-packages can
// build the same pattern the core does.
func LikePattern(op FilterOperator, value any) string {
	var s string
	if value != nil {
		s = fmt.Sprint(value)
	}
	// The escape character goes first, or it would escape the escapes we add.
	s = strings.ReplaceAll(s, LikeEscapeChar, LikeEscapeChar+LikeEscapeChar)
	s = strings.ReplaceAll(s, "%", LikeEscapeChar+"%")
	s = strings.ReplaceAll(s, "_", LikeEscapeChar+"_")

	switch op {
	case OpContains:
		return "%" + s + "%"
	case OpStartsWith:
		return s + "%"
	case OpEndsWith:
		return "%" + s
	}
	return ""
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

	// Forced marks a filter the server imposed rather than one the client asked
	// for — a tenant scope, an ownership scope, a soft-delete guard. It changes
	// nothing about how the filter reads; it decides whether the filter also
	// constrains an update or a delete.
	//
	// The distinction is necessary because both kinds share Query.Filters. A
	// client's ?filter= must keep being ignored on a write (it always was, and a
	// stray query parameter turning a PATCH into a 404 would be a surprise), while
	// a server-imposed scope must be honoured there or a caller can update and
	// delete rows the same filter hides from their reads.
	//
	// db.Tenancy and db.ForceFilter set it. Set it on a hand-built FilterExpr when
	// the filter expresses who may touch the row rather than which rows were asked
	// for; the DB step then refuses a write to a record the filter excludes.
	Forced bool
}

// forcedFilters returns the server-imposed filters among fs, or nil when there
// are none. nil is the overwhelmingly common case — no tenancy, nothing to
// enforce — and is what lets the write path skip its scope check entirely.
func forcedFilters(fs []*FilterExpr) []*FilterExpr {
	var out []*FilterExpr
	for _, f := range fs {
		if f != nil && f.Forced {
			out = append(out, f)
		}
	}
	return out
}

// nestedForcedFilters returns the server-imposed filters among fs that scope
// through a parent relation rather than a column of the model itself — what
// db.ForceFilterVia builds. They are the only filters with a foreign key to
// check, so isolating them is what lets every other write skip that check.
func nestedForcedFilters(fs []*FilterExpr) []*FilterExpr {
	var out []*FilterExpr
	for _, f := range fs {
		if f != nil && f.Forced && f.IsNested {
			out = append(out, f)
		}
	}
	return out
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
	if err := checkFilterValue(op, value); err != nil {
		return nil, err
	}

	expr := &FilterExpr{Operator: op, Value: value, Group: -1}

	if strings.Contains(fieldPath, ".") {
		return resolveNestedFilter(expr, fieldPath, model, reg)
	}
	return resolveFlatFilter(expr, fieldPath, model)
}

// checkFilterValue rejects a value whose shape the operator cannot use. Each of
// these would otherwise reach the adapter and produce SQL that is broken or, worse,
// quietly wrong.
func checkFilterValue(op FilterOperator, value any) error {
	switch op {
	case OpBetween:
		if value == nil || len(SplitCSV(fmt.Sprint(value))) != 2 {
			return fmt.Errorf("operator %q requires two comma-separated values (e.g. amount:between:100,500)", op)
		}

	// An empty list has no meaningful SQL form: "role:in:" (and "role:in:,,",
	// whose entries all drop out) would otherwise reach the adapter as zero
	// values and emit "role IN ()" — a syntax error on every driver, so a client
	// could provoke a 500 at will (BUG-7).
	case OpIn, OpNotIn:
		if value == nil || len(SplitCSV(fmt.Sprint(value))) == 0 {
			return fmt.Errorf("operator %q requires at least one comma-separated value (e.g. role:in:admin,editor)", op)
		}

	// The substring operators build their pattern from the value. With none at all
	// ("name:contains") there is nothing to escape and the pattern would be a bare
	// wildcard matching every row — say so rather than quietly returning the table.
	case OpContains, OpStartsWith, OpEndsWith:
		if value == nil {
			return fmt.Errorf("operator %q requires a value (e.g. name:contains:acme)", op)
		}
	}
	return nil
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
