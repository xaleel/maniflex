package maniflex

import (
	"fmt"
	"reflect"
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

	// The JSON operators ask whether a JSON column holds a value. They are the
	// answer to a question `contains` looked like it answered and did not:
	// `contains` compiles to a substring LIKE, which against a JSON array matches
	// across element boundaries on SQLite (a search for "cat-1" finds ["cat-10"])
	// and cannot be applied to JSONB at all on Postgres — silently wrong results in
	// development, a failed query in production.
	//
	// They require the column to carry mfx:"json_array" or mfx:"json_object",
	// which also decides what the operator means: on an array, element membership
	// (`tags:has:urgent`); on an object, a key=value pair (`meta:has:name=John`).
	// Both sides are compared JSON-encoded, so the string "5" and the number 5 are
	// different values on both drivers rather than the same one on one of them.
	OpHas    FilterOperator = "has"     // JSON column holds this element / pair
	OpNotHas FilterOperator = "not_has" // …and its negation

	// The *_field operators compare two columns of the same model instead of
	// comparing a column to a literal — "paid_amount >= amount_due". Their value
	// is a field name, carried in FilterExpr.ValueField.
	//
	// They are separate operators rather than a sigil on the value ("gte:$col")
	// because that is what makes the reading unambiguous by construction. With a
	// sigil, whether the right-hand side is a column depends on the text a client
	// sent, so a literal that happens to look like a marker silently becomes a
	// column comparison and returns a confident, wrong list — and dodging that
	// needs an escaping rule in a parser that has none. Here the six take a column
	// and the other sixteen take a literal, and no input can cross between them.
	//
	// Scope: both columns live on the model being filtered, both are filterable,
	// and both hold the same kind of value. Arithmetic on the right-hand side is
	// deliberately absent — "a >= b + c" needs an expression grammar, which is a
	// separate feature.
	OpEqField  FilterOperator = "eq_field"  // field = other field
	OpNeqField FilterOperator = "neq_field" // field != other field
	OpGtField  FilterOperator = "gt_field"  // field > other field
	OpGteField FilterOperator = "gte_field" // field >= other field
	OpLtField  FilterOperator = "lt_field"  // field < other field
	OpLteField FilterOperator = "lte_field" // field <= other field
)

// allFilterOperators is every operator the query builder implements, in the
// order the generated documentation presents them.
//
// It is the single source: validOperators is derived from it, and the ?filter=
// parameter's description in the OpenAPI spec is generated from it. Those were
// two hand-maintained lists a few hundred lines apart, so an operator could be
// implemented and undocumented — which is what a client reads the spec to find
// out, and the one place the omission is invisible.
var allFilterOperators = []FilterOperator{
	OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte,
	OpLike, OpILike,
	OpContains, OpStartsWith, OpEndsWith,
	OpHas, OpNotHas,
	OpIn, OpNotIn, OpIsNull, OpNotNull, OpBetween,
	OpEqField, OpNeqField, OpGtField, OpGteField, OpLtField, OpLteField,
}

var validOperators = func() map[FilterOperator]bool {
	m := make(map[FilterOperator]bool, len(allFilterOperators))
	for _, op := range allFilterOperators {
		m[op] = true
	}
	return m
}()

// filterOperatorList renders allFilterOperators for the generated spec.
func filterOperatorList() string {
	names := make([]string, len(allFilterOperators))
	for i, op := range allFilterOperators {
		names[i] = string(op)
	}
	return strings.Join(names, ", ")
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

// rejectNilFilters reports a nil *FilterExpr in fs, naming where it came from.
//
// A nil is reachable only from Go — the URL parser never produces one — but it
// is easy to produce there, and it is refused rather than skipped. ViaFilter
// returns (nil, error) on every failure path and says in its own doc that
// skipping such an error "would leave the request unscoped rather than refused";
// a caller who ignores it appends exactly this nil, and the thing it stood for
// was a scope. Carrying on would run the query with less scope than the code
// reads as applying, which is the failure a dropped forced filter has every
// time (audit O1).
//
// Downstream code still skips nils where it iterates a filter list, so a path
// that never reaches a boundary degrades to an ignored entry rather than a
// panic. This is what turns that into a diagnosis.
func rejectNilFilters(fs []*FilterExpr, source string) error {
	for i, f := range fs {
		if f == nil {
			return fmt.Errorf(
				"maniflex: %s contains a nil filter at index %d — a *FilterExpr is nil when the "+
					"call that built it failed, so check the error from ViaFilter (or whatever "+
					"produced it) rather than appending its result unconditionally",
				source, i)
		}
	}
	return nil
}

// validateFilterFields rejects a filter naming a field the model does not have.
//
// It is the field half of the same problem validateFilterOperators covers for
// operators: a filter parsed from a URL had its field resolved and rewritten by
// resolveFlatFilter, so it always names a real column, while one built in Go
// carries whatever was typed. That used to reach the adapter verbatim and fail
// as an unknown-column SQL error — a 500 whose message named a column the
// author never wrote, since the json spelling they did write is not what the
// table calls it.
//
// The adapters fail such a filter closed (a false predicate, so a misspelt
// forced filter narrows to nothing rather than widening to everything). That is
// the safe answer but a silent one: an empty list looks exactly like no matching
// data. This is the layer that can say why instead.
//
// Nested and locale filters are skipped: their Field names a column on a joined
// relation or a key inside a JSON document, neither of which is a field on this
// model.
func validateFilterFields(model *ModelMeta, fs []*FilterExpr) error {
	if model == nil {
		return nil
	}
	for _, f := range fs {
		if f == nil || f.IsNested || f.IsLocale {
			continue
		}
		if model.ResolveFilterField(f.Field) == nil {
			return fmt.Errorf(
				"maniflex: filter names %q, which is not a field on model %s — a FilterExpr built "+
					"in Go is not parsed, so the field is whatever was typed; use the column's DB "+
					"name or its json name",
				f.Field, model.Name)
		}
		// A *_field filter names a second column, which reaches the renderer just
		// as unparsed as the first. Left unchecked it fails closed to 1=0 — safe,
		// but indistinguishable from a table with no matching rows.
		if f.ValueField != "" && model.ResolveFilterField(f.ValueField) == nil {
			return fmt.Errorf(
				"maniflex: filter on %q compares against %q, which is not a field on model %s — "+
					"a FilterExpr built in Go is not parsed, so the field is whatever was typed; "+
					"use the column's DB name or its json name",
				f.Field, f.ValueField, model.Name)
		}
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
	Field    string         // DB column name on the primary table, or its json name
	Operator FilterOperator

	// Value is the raw string a URL carries, or any Go value when the filter is
	// built programmatically. It is coerced against the column it targets — see
	// NormalizeFilterValue — so a time.Time and a bool need no pre-formatting.
	//
	// For the multi-value operators (in, not_in, between) a []string or []any is
	// taken as written; anything else takes the comma-separated form. A slice is
	// the safer spelling, since a value containing a comma stays one element.
	Value any

	// ValueField names a second column on the same model that this filter
	// compares against, instead of comparing Field to a literal Value. It is set
	// only by the *_field operators and is empty for every other filter.
	//
	// It is a field of its own rather than a column name stuffed into Value
	// because Value is coerced against the target column by NormalizeFilterValue.
	// Handed the string "amount_due" for an int64 column, that coercion has
	// nothing to measure and would pass a column name along as a literal — the
	// value would bind, the comparison would be against the text rather than the
	// column, and nothing would say so. Keeping it separate means normalisation
	// is skipped rather than fooled.
	//
	// Like Field, it accepts either the DB column name or the json name.
	ValueField string

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

// scopeColumns turns a set of scope filters into the columns a row must carry to
// satisfy them, and reports the first filter it cannot.
//
// An equality on a plain column says two things at once: "only rows where field =
// value" and "a row you create has field = value". That second reading is what
// lets a scope be *written* as well as read, and it is the only shape that has
// one — an operator that is not equality, a nested filter (the value lives on
// another table), a locale filter, an OR group (any one of several values would
// do) name no single value to store.
//
// A caller that cannot satisfy a scope must refuse the write rather than perform
// it: the row would otherwise be one its own author's next read cannot see.
// Callers phrase their own error from the returned filter, because "why can this
// not be stored" reads differently for a create through a scoped Action than for
// provisioning a scoped singleton.
func scopeColumns(filters []*FilterExpr) (map[string]any, *FilterExpr) {
	out := make(map[string]any, len(filters))
	for _, f := range filters {
		if f == nil {
			continue
		}
		if f.Operator != OpEq || f.IsNested || f.IsLocale || f.Group > 0 {
			return nil, f
		}
		out[f.Field] = f.Value
	}
	return out, nil
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
		if f == nil || f.Group <= 0 {
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
//	paid_amount:gte_field:amount_due   (compares two columns of this model)
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

	// A *_field filter resolves both sides as columns, so it takes neither the
	// nested path (its right-hand side is not a value on another table) nor the
	// flat one (which would coerce the column name as a literal).
	if _, isFieldOp := fieldComparisonOp(op); isFieldOp {
		return resolveFieldComparison(expr, fieldPath, fmt.Sprint(value), model)
	}
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

	// The *_field operators take a column name, not a literal. With none at all
	// ("paid_amount:gte_field") there is no right-hand side, and a blank one
	// would resolve to no column and fail closed as an empty list — say so here
	// instead.
	case OpEqField, OpNeqField, OpGtField, OpGteField, OpLtField, OpLteField:
		if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
			return fmt.Errorf(
				"operator %q requires a field name (e.g. paid_amount:gte_field:amount_due)", op)
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
		return nil, fmt.Errorf("field %q on model %s is not filterable (%s)",
			fieldPath, model.Name, howToAllow(f.Tags.DBName, "filterable"))
	}
	if err := checkJSONColumnFilter(f, fieldPath, model.Name, expr.Operator, expr.Value); err != nil {
		return nil, err
	}
	expr.Field = f.Tags.DBName
	// On a time-typed column the value is compared as TEXT on SQLite, so a raw
	// client string ("…12:00:00Z", a zone offset, a short fraction) can misorder
	// against the fixed-width form the write path stores. Re-emit a well-formed
	// timestamp in that same form; leave date-only and non-time values untouched.
	if isTimeType(f.Type) {
		expr.Value = canonicalizeTimeFilterValue(expr.Operator, expr.Value)
	}
	return expr, nil
}

func resolveNestedFilter(expr *FilterExpr, fieldPath string, model *ModelMeta, reg RegistryAccessor) (*FilterExpr, error) {
	dot := strings.SplitN(fieldPath, ".", 2)
	relKey := dot[0]
	nestedField := dot[1]

	// Check if the left side is a locale field before looking for a relation.
	if f := model.FieldByJSONName(relKey); f != nil && f.Tags.Locale {
		if !f.Tags.Filterable {
			return nil, fmt.Errorf("field %q on model %s is not filterable (%s)",
				relKey, model.Name, howToAllow(f.Tags.DBName, "filterable"))
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
		return nil, fmt.Errorf("field %q on related model %s is not filterable (%s)",
			nestedField, relMeta.Name, howToAllow(nf.Tags.DBName, "filterable"))
	}

	expr.Field = fieldPath
	expr.IsNested = true
	expr.RelationKey = relKey
	expr.RelationModel = rel.RelatedModel
	expr.RelationTable = relMeta.TableName
	expr.RelationFK = rel.FKColumn
	expr.NestedField = nf.Tags.DBName
	if isTimeType(nf.Type) {
		expr.Value = canonicalizeTimeFilterValue(expr.Operator, expr.Value)
	}

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
		return SortExpr{}, fmt.Errorf("field %q on related model %s is not sortable (%s)",
			nestedField, relMeta.Name, howToAllow(nf.Tags.DBName, "sortable"))
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
// validateJSONColumnTags checks that mfx:"json_array" / mfx:"json_object" agree
// with the field they are on.
//
// The tag is what the `has` operator compiles from, and it is the only thing
// that knows: a JSON column is JSONB on Postgres but plain TEXT on SQLite, so
// the SQL type cannot be asked. That makes the tag load-bearing and unchecked
// by anything else — a json_array on a string column would build json_each over
// text, which returns nothing on SQLite and errors on Postgres, with no hint
// that a tag is the reason.
func (m *ModelMeta) validateJSONColumnTags() error {
	for i := range m.Fields {
		f := &m.Fields[i]
		if !f.Tags.JSONArray && !f.Tags.JSONObject {
			continue
		}
		if f.Tags.JSONArray && f.Tags.JSONObject {
			return fmt.Errorf(
				"maniflex: model %q field %q carries both json_array and json_object; "+
					"a column holds one or the other, and has compiles differently for each",
				m.Name, f.Tags.JSONName)
		}
		t := f.Type
		for t != nil && t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == nil {
			continue
		}
		tag, ok := "json_array", jsonArrayKind(t)
		if f.Tags.JSONObject {
			tag, ok = "json_object", jsonObjectKind(t)
		}
		if !ok {
			return fmt.Errorf(
				"maniflex: model %q field %q is tagged %s but its Go type is %s; "+
					"the tag says what the column holds and has is compiled from it, so a "+
					"type that cannot hold that produces a filter which never matches",
				m.Name, f.Tags.JSONName, tag, f.Type)
		}
	}
	return nil
}

// jsonArrayKind reports whether t serialises as a JSON array. A byte slice is
// excluded: it marshals to a base64 string, so walking its elements would walk
// bytes rather than the values the tag promises.
func jsonArrayKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return t.Elem().Kind() != reflect.Uint8
	}
	return false
}

// jsonObjectKind reports whether t serialises as a JSON object. time.Time is a
// struct and marshals to a string, so it is excluded by name the way it is
// everywhere else the framework reasons about kinds.
func jsonObjectKind(t reflect.Type) bool {
	if isTimeType(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Map, reflect.Struct:
		return true
	}
	return false
}

// checkJSONColumnFilter refuses the two filter shapes a JSON column makes wrong,
// in both directions.
//
// has/not_has ask what a JSON document holds, so they need a column that holds
// one. Without the tag the builder has no JSON to look inside and renders a
// predicate that matches nothing — a filter that silently returns an empty list
// is the failure this operator was added to remove, not one to reintroduce.
//
// contains/starts_with/ends_with are the original defect (todo/ASKS.md issue 1):
// they compile to a substring LIKE, and against a serialised JSON document that
// matched across element boundaries on SQLite — a search for "cat-1" answering
// 200 with a row holding ["cat-10"] — while on Postgres LIKE cannot be applied to
// JSONB at all. The same query was quietly wrong in development and a hard error
// in production. Nothing correct depends on the old behaviour, so it is refused
// rather than kept: a 400 naming the operator that works is the one outcome that
// cannot be mistaken for an answer.
func checkJSONColumnFilter(f *FieldMeta, fieldPath, modelName string, op FilterOperator, value any) error {
	isJSON := f.Tags.JSONArray || f.Tags.JSONObject
	switch op {
	case OpHas, OpNotHas:
		if !isJSON {
			return fmt.Errorf(
				"operator %q on field %q of model %s asks what a JSON column holds, and %q is "+
					"not tagged as one; add mfx:%q for a JSON array or mfx:%q for a JSON object",
				op, fieldPath, modelName, fieldPath, "json_array", "json_object")
		}
		return checkJSONHasValue(f, fieldPath, op, value)
	case OpContains, OpStartsWith, OpEndsWith:
		if isJSON {
			return fmt.Errorf(
				"operator %q on field %q of model %s substring-matches the serialised JSON: it "+
					"matches across element boundaries on SQLite and cannot run at all on "+
					"Postgres. Use %q to ask whether the column holds a value",
				op, fieldPath, modelName, OpHas)
		}
	}
	return nil
}

// checkJSONHasValue holds the has value to the shape its column gives it: a whole
// element for an array, a key=value pair for an object.
//
// The object key is bound into a JSON path by the query builder rather than
// spliced into it, so this allowlist is not what stops an injection — it is what
// stops a key the path cannot express reaching SQL as a malformed one, and what
// makes an unsupported shape say so. It matches the rule locale keys are held to
// for the same reason (SEC-1).
func checkJSONHasValue(f *FieldMeta, fieldPath string, op FilterOperator, value any) error {
	s := ""
	if value != nil {
		s = fmt.Sprint(value)
	}
	if s == "" {
		return fmt.Errorf("operator %q on field %q requires a value (e.g. %s:%s:urgent)",
			op, fieldPath, fieldPath, op)
	}
	if !f.Tags.JSONObject {
		return nil
	}
	key, _, found := strings.Cut(s, "=")
	if !found {
		return fmt.Errorf(
			"operator %q on the JSON object field %q takes key=value (e.g. %s:%s:name=John), got %q",
			op, fieldPath, fieldPath, op, s)
	}
	if strings.Contains(key, ".") {
		return fmt.Errorf(
			"operator %q on field %q does not yet take a path: %q addresses a nested value, "+
				"and only a top-level key is supported",
			op, fieldPath, key)
	}
	if !isJSONObjectKey(key) {
		return fmt.Errorf(
			"invalid JSON object key %q in filter on %q: must be a key identifier "+
				"([A-Za-z_][A-Za-z0-9_-]*)", key, fieldPath)
	}
	return nil
}

// isJSONObjectKey reports whether s is a JSON object key a has filter may name.
// The same rule locale keys are held to, and for the same reason: the key is
// targeted into a JSON-path expression, so what may appear in one is decided at
// the door rather than in the query builder.
func isJSONObjectKey(s string) bool { return isLocaleKey(s) }

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
