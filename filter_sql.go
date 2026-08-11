package maniflex

import (
	"fmt"
	"sort"
	"strings"
)

// PlaceholderBinder binds a value into a statement and returns the placeholder
// text that stands for it — "?" on SQLite, "$N" on Postgres.
//
// Implementations must append in call order. SQLite binds "?" positionally by
// where it appears in the SQL text, so a binder that registered a placeholder
// out of text order would scramble every argument after it without producing a
// syntax error. db/sqlcore.PlaceholderBuilder is the shipped implementation.
type PlaceholderBinder interface {
	Add(v any) string
}

// BuildFilterSQL renders filters into the body of a WHERE clause — "a AND (b OR
// c)", with no leading WHERE and no trailing space. An empty or entirely nil
// filter list renders "".
//
// This is the framework's only filter renderer. There used to be two (audit
// AG-5): this one, used by the aggregate path, and a near-copy inside
// db/sqlcore used by every other query. They had to be kept in agreement by
// hand, and were not — every capability one grew that the other did not shipped
// as a silently wrong answer, on an endpoint whose whole job is reporting
// numbers. AG-1 (grouped OR ignored), AG-4 (between rendered as = 'lo,hi') and
// QF-3's aggregate half were all the same bug wearing different clothes, and
// three more divergences were still live when the two were merged: a Go slice
// bound to IN as the literal "[a b]", an empty NOT IN that matched everything
// on one path and nothing on the other, and locale filters that the aggregate
// compared against the raw JSON column.
//
// It is exported because db/sqlcore cannot be imported from here — the
// dependency runs the other way — and because a third-party DBAdapter would
// otherwise have to write its own renderer, which is this same problem at a
// larger scale.
//
// Grouping: a filter with Group <= 0 (the FilterExpr zero value included) is
// its own AND clause; filters sharing a Group >= 1 are OR-ed together. Groups
// are emitted in ascending order so the SQL is deterministic.
//
// Nested filters render as "relation"."column", which requires the caller's
// FROM clause to have joined that relation. A caller that does not join —
// ctx.Aggregate — must reject nested filters before calling this.
func BuildFilterSQL(model *ModelMeta, filters []*FilterExpr, driver DriverType, p PlaceholderBinder) string {
	ungrouped, groups, groupOrder := partitionFilters(filters)

	parts := make([]string, 0, len(ungrouped)+len(groupOrder))
	for _, f := range ungrouped {
		parts = append(parts, filterCond(model, f, driver, p))
	}
	for _, gid := range groupOrder {
		exprs := groups[gid]
		if len(exprs) == 1 {
			parts = append(parts, filterCond(model, exprs[0], driver, p))
			continue
		}
		orParts := make([]string, len(exprs))
		for i, f := range exprs {
			orParts[i] = filterCond(model, f, driver, p)
		}
		parts = append(parts, "("+strings.Join(orParts, " OR ")+")")
	}
	return strings.Join(parts, " AND ")
}

// partitionFilters splits filters into the ungrouped ones and the OR groups,
// with the group keys sorted so the rendered SQL does not depend on map order.
//
// Nil entries are dropped. The adapter's copy of this loop read f.Group off the
// pointer without checking, so one nil in a programmatically built slice
// panicked there and was skipped on the aggregate path.
func partitionFilters(filters []*FilterExpr) ([]*FilterExpr, map[int][]*FilterExpr, []int) {
	var ungrouped []*FilterExpr
	groups := make(map[int][]*FilterExpr)
	var order []int

	for _, f := range filters {
		if f == nil {
			continue
		}
		if f.Group <= 0 {
			ungrouped = append(ungrouped, f)
			continue
		}
		if _, seen := groups[f.Group]; !seen {
			order = append(order, f.Group)
		}
		groups[f.Group] = append(groups[f.Group], f)
	}
	sort.Ints(order)
	return ungrouped, groups, order
}

// filterCond renders one filter as a predicate.
//
// A plain filter — not nested, not locale — is resolved against the model
// first, which does two things a filter parsed from a URL already had done for
// it and a filter built in Go never did.
//
// It settles the column: ResolveFilterField accepts the json spelling as well
// as the DB one, so a caller who wrote the name the model publishes gets the
// column rather than an identifier no table has.
//
// And it settles the value: NormalizeFilterValue puts it in the form that
// column binds against, which is what makes a Go-built filter and a URL one
// mean the same thing. A time.Time has to reach SQLite in the same fixed-width
// form the write path stored, and a bool column has to receive a real bool
// rather than the word "false", which binds as TEXT against an INTEGER column
// and can never compare equal.
//
// A field the model does not have fails closed, for the reason filterPredicate
// gives at length for an unimplemented operator.
//
// A *_field filter compares two columns and binds nothing; it is settled before
// the value path, since there is no value to normalise.
func filterCond(model *ModelMeta, f *FilterExpr, driver DriverType, p PlaceholderBinder) string {
	if f.IsLocale || f.IsNested {
		return filterPredicate(filterColumn(model, f, driver, p), f.Operator, f.Value, driver, p)
	}
	fm := model.ResolveFilterField(f.Field)
	if fm == nil {
		return falsePredicate
	}
	col := Quote(model.TableName) + "." + Quote(fm.Tags.DBName)
	if sqlOp, ok := fieldComparisonOp(f.Operator); ok {
		return fieldCond(model, col, sqlOp, f.ValueField)
	}
	return filterPredicate(col, f.Operator, NormalizeFilterValue(fm, f.Operator, f.Value), driver, p)
}

// filterColumn renders the left-hand side of a locale or nested predicate.
func filterColumn(model *ModelMeta, f *FilterExpr, driver DriverType, p PlaceholderBinder) string {
	if f.IsLocale {
		col := Quote(model.TableName) + "." + Quote(f.Field)
		if driver == Postgres {
			// Bind the locale key as a parameter so it can never break out of
			// the JSON-path expression (SEC-1). The ::text cast pins the ->>
			// overload to object-field access regardless of how the driver
			// types the parameter.
			return col + "->>" + p.Add(f.LocaleKey) + "::text"
		}
		// SQLite: bind the whole "$.<key>" path as a parameter.
		return "json_extract(" + col + ", " + p.Add("$."+f.LocaleKey) + ")"
	}
	return Quote(f.RelationKey) + "." + Quote(f.NestedField)
}

// fieldComparisonOp maps a *_field operator onto its SQL comparison, and reports
// whether op is one at all. It is the single place that knows the six, so the
// parser and the renderer cannot disagree about the set.
func fieldComparisonOp(op FilterOperator) (string, bool) {
	switch op {
	case OpEqField:
		return "=", true
	case OpNeqField:
		return "!=", true
	case OpGtField:
		return ">", true
	case OpGteField:
		return ">=", true
	case OpLtField:
		return "<", true
	case OpLteField:
		return "<=", true
	}
	return "", false
}

// fieldCond renders a column-to-column comparison, binding no parameter at all.
//
// Binding nothing is safe only because the right-hand column is resolved through
// the model exactly as the left one is: what reaches the SQL text is a DB name
// the registry vouches for, never the string a client sent. That is the same
// property the sealed Expr/Col type has on the aggregate side, and it is the
// whole argument for why an identifier may be concatenated here when a value
// never may be.
//
// An unresolvable name fails closed, for the reason filterPredicate gives at
// length: a comparison that cannot be built must match nothing, because the
// alternative deletes the filter — and a deleted filter on a forced scope is not
// a wider scope, it is no scope.
func fieldCond(model *ModelMeta, col, sqlOp, valueField string) string {
	rhs := model.ResolveFilterField(valueField)
	if rhs == nil {
		return falsePredicate
	}
	return col + " " + sqlOp + " " + Quote(model.TableName) + "." + Quote(rhs.Tags.DBName)
}

// filterPredicate renders one operator against an already-rendered column.
func filterPredicate(col string, op FilterOperator, val any, driver DriverType, p PlaceholderBinder) string {
	switch op {
	case OpEq:
		return fmt.Sprintf("%s = %s", col, p.Add(val))
	case OpNeq:
		return fmt.Sprintf("%s != %s", col, p.Add(val))
	case OpGt:
		return fmt.Sprintf("%s > %s", col, p.Add(val))
	case OpGte:
		return fmt.Sprintf("%s >= %s", col, p.Add(val))
	case OpLt:
		return fmt.Sprintf("%s < %s", col, p.Add(val))
	case OpLte:
		return fmt.Sprintf("%s <= %s", col, p.Add(val))
	case OpIsNull:
		return col + " IS NULL"
	case OpNotNull:
		return col + " IS NOT NULL"
	case OpLike:
		return fmt.Sprintf("%s LIKE %s", col, p.Add(val))
	case OpILike:
		if driver == Postgres {
			return fmt.Sprintf("%s ILIKE %s", col, p.Add(val))
		}
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", col, p.Add(val))
	case OpContains, OpStartsWith, OpEndsWith:
		return substringPredicate(col, op, val, driver, p)
	case OpIn:
		return inPredicate(col, val, p, false)
	case OpNotIn:
		return inPredicate(col, val, p, true)
	case OpBetween:
		return betweenPredicate(col, val, p)
	}
	// Fail closed. An operator this switch does not implement cannot be turned
	// into a predicate, and the choice is between a condition that matches
	// nothing and one that matches everything. Matching everything deletes the
	// filter: a list returns the whole table, and a forced filter — a tenant
	// scope — stops scoping, on reads and on the writes that read back through
	// it. Matching nothing is wrong too, but it is wrong in the direction that
	// shows up on the first request rather than the first breach. The DB step
	// rejects such a filter outright (validateFilterOperators); this is the
	// backstop for the paths that do not run it, and for a filter built after
	// it ran.
	return falsePredicate
}

// substringPredicate builds contains / starts_with / ends_with: a
// case-insensitive LIKE against a pattern whose metacharacters are escaped, so
// a value of "50%" matches the literal "50%" instead of everything starting
// with 50.
//
// The ESCAPE clause is spelled out because the drivers disagree without it —
// SQLite has no escape character by default, Postgres has a backslash — and the
// same filter must mean the same thing on both.
func substringPredicate(col string, op FilterOperator, val any, driver DriverType, p PlaceholderBinder) string {
	pattern := LikePattern(op, val)
	if driver == Postgres {
		return fmt.Sprintf("%s ILIKE %s ESCAPE '\\'", col, p.Add(pattern))
	}
	return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s) ESCAPE '\\'", col, p.Add(pattern))
}

// betweenPredicate expands a "lo,hi" value into "col >= lo AND col <= hi". Both
// bounds are required upstream in ParseFilterParam, so only a hand-built
// FilterExpr reaches here malformed; it degrades to a false predicate rather
// than emitting broken SQL. False, not true, for the reason filterPredicate
// gives: a between with one bound is a filter whose author meant to exclude
// something, and answering "everything matches" is the one reading that cannot
// be right.
func betweenPredicate(col string, val any, p PlaceholderBinder) string {
	bounds := splitFilterList(val)
	if len(bounds) != 2 {
		return falsePredicate
	}
	return fmt.Sprintf("(%s >= %s AND %s <= %s)", col, p.Add(bounds[0]), col, p.Add(bounds[1]))
}

// inPredicate expands a value into "col IN (?, ?, ?)".
//
// An empty set has no SQL form — "IN ()" is a syntax error on every driver — so
// it renders as a constant instead: an empty IN matches nothing and an empty
// NOT IN matches everything, which is what the set semantics say. The aggregate
// builder used to answer 1=0 to both, so an empty NOT IN returned every row
// from the list endpoint and none from the aggregate.
func inPredicate(col string, val any, p PlaceholderBinder, negate bool) string {
	vals := splitFilterList(val)
	if len(vals) == 0 {
		if negate {
			return truePredicate
		}
		return falsePredicate
	}
	placeholders := make([]string, len(vals))
	for i, v := range vals {
		placeholders[i] = p.Add(v)
	}
	kw := "IN"
	if negate {
		kw = "NOT IN"
	}
	return fmt.Sprintf("%s %s (%s)", col, kw, strings.Join(placeholders, ", "))
}

// splitFilterList resolves the value of a multi-value operator — IN, NOT IN,
// BETWEEN — into its elements.
//
// A slice is taken as written, which is the form a Go-built FilterExpr uses.
// The adapter's copy went through fmt.Sprint first, so []string{"a","b"} became
// the single literal "[a b]" and matched nothing; the aggregate's copy handled
// slices but returned nothing at all for a plain number, so IN 42 was 1=0
// there. Everything that is not a slice takes the CSV path, which is what a URL
// carries and what any scalar formats into.
func splitFilterList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	parts := SplitCSV(fmt.Sprint(v))
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, p)
	}
	return out
}

// The two constant predicates a builder emits when it cannot render the filter
// it was given. Spelled once so every path answers an impossible filter the
// same way.
const (
	falsePredicate = "1=0"
	truePredicate  = "1=1"
)

// Quote wraps an identifier in double quotes and escapes any embedded quote by
// doubling it (ANSI SQL §5.2). Both drivers accept this encoding.
//
// Exported for the same reason as BuildFilterSQL: an adapter building SQL needs
// to quote identifiers the way the framework does, and the alternative to
// sharing this is every adapter carrying its own copy.
func Quote(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
