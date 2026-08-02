package maniflex

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AggregateOp is the SQL aggregate function applied to a column.
type AggregateOp string

const (
	AggCount         AggregateOp = "count"
	AggCountDistinct AggregateOp = "count_distinct"
	AggSum           AggregateOp = "sum"
	AggAvg           AggregateOp = "avg"
	AggMin           AggregateOp = "min"
	AggMax           AggregateOp = "max"
)

// AggregateField selects one aggregate to compute. Field is the DB column to
// aggregate over; leave it empty for AggCount to mean COUNT(*). As overrides
// the result column alias; defaults to "<op>_<field>" (or "count" for
// COUNT(*)).
type AggregateField struct {
	Op    AggregateOp
	Field string
	As    string
}

// HavingClause filters aggregated results by an aggregate alias. Operator is
// the same set used by FilterExpr (eq, gt, gte, lt, lte, neq); set ops, like,
// and null checks are rejected since they make no sense on a numeric
// aggregate result.
type HavingClause struct {
	Alias    string
	Operator FilterOperator
	Value    any
}

// AggregateQuery is the input to ctx.Aggregate. All slices are optional except
// Select. GroupBy referenced columns must be DB column names on the model;
// HAVING aliases must match an entry in Select; OrderBy may reference either
// an aggregate alias or a GroupBy column.
type AggregateQuery struct {
	Select  []AggregateField
	GroupBy []string
	Where   []*FilterExpr
	Having  []HavingClause
	OrderBy []SortExpr
	Limit   int
}

// aggregateValidationError distinguishes a validated AggregateQuery failure from an
// adapter/database failure without expanding the public error API.
type aggregateValidationError struct {
	err error
}

func (e *aggregateValidationError) Error() string { return e.err.Error() }
func (e *aggregateValidationError) Unwrap() error { return e.err }

func aggregateQueryError(err error) error {
	return &aggregateValidationError{err: err}
}

func aggregateQueryErrorf(format string, args ...any) error {
	return aggregateQueryError(fmt.Errorf(format, args...))
}

// Aggregate runs a structured aggregation query against the named model. It
// validates field names against the model's registered DB columns so an
// invalid input fails fast with a clear error rather than producing a SQL
// syntax error from the driver.
//
// The query participates in ctx.Tx when one is active; otherwise it uses the
// adapter's read pool. The returned rows are one map per group; aggregate
// values are accessed by alias.
//
//	rows, err := ctx.Aggregate("Order", maniflex.AggregateQuery{
//	    Select: []maniflex.AggregateField{
//	        {Op: maniflex.AggCount, As: "n"},
//	        {Op: maniflex.AggSum, Field: "total", As: "revenue"},
//	    },
//	    GroupBy: []string{"status"},
//	    Where:   []*maniflex.FilterExpr{{Field: "created_at", Operator: maniflex.OpGte, Value: "2026-01-01"}},
//	    OrderBy: []maniflex.SortExpr{{DBName: "revenue", Direction: maniflex.SortDesc}},
//	    Limit:   100,
//	})
func (c *ServerContext) Aggregate(modelName string, agg AggregateQuery) ([]Row, error) {
	if c.reg == nil {
		return nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}
	meta, ok := c.reg.Get(modelName)
	if !ok {
		return nil, fmt.Errorf("maniflex: model %q is not registered", modelName)
	}
	// An ActionScope AND-s into the WHERE. An aggregate is a read like any other,
	// and an unscoped one leaks across the scope in summary form — a per-tenant
	// SUM computed over every tenant's rows is still a disclosure. Copied rather
	// than appended in place so a caller reusing one AggregateQuery does not
	// accumulate the scope on each call.
	if sf := c.scopeFilters(); len(sf) > 0 {
		merged := make([]*FilterExpr, 0, len(agg.Where)+len(sf))
		merged = append(merged, agg.Where...)
		merged = append(merged, sf...)
		agg.Where = merged
	}
	if len(agg.Select) == 0 {
		return nil, aggregateQueryErrorf("maniflex: AggregateQuery.Select must contain at least one field")
	}

	driver := c.DriverType()
	pb := newAggPH(driver)

	// SELECT
	selectParts := make([]string, 0, len(agg.Select)+len(agg.GroupBy))
	aliases := make(map[string]bool, len(agg.Select))
	aliasField := make(map[string]AggregateField, len(agg.Select))
	for _, f := range agg.Select {
		expr, alias, err := aggregateSelectSQL(meta, f, pb)
		if err != nil {
			return nil, aggregateQueryError(err)
		}
		if aliases[alias] {
			return nil, aggregateQueryErrorf("maniflex: duplicate aggregate alias %q", alias)
		}
		aliases[alias] = true
		aliasField[alias] = f
		selectParts = append(selectParts, expr+" AS "+aggQuote(alias))
	}
	// Always include the GROUP BY columns in the SELECT so the caller sees
	// which group each row belongs to.
	for _, col := range agg.GroupBy {
		if meta.FieldByDBName(col) == nil {
			return nil, aggregateQueryErrorf("maniflex: GroupBy column %q does not exist on model %q", col, modelName)
		}
		selectParts = append(selectParts, aggQuote(meta.TableName)+"."+aggQuote(col))
	}

	// WHERE — validate field names against the model so an unknown one is a
	// clean error rather than a SQL fault. Either spelling resolves, matching
	// the list path; aggBuildWhere fails an unresolvable filter closed, so this
	// is the layer that can say why rather than the one holding the line.
	for _, f := range agg.Where {
		if !f.IsNested && meta.ResolveFilterField(f.Field) == nil {
			return nil, aggregateQueryErrorf("maniflex: Where filter references unknown column %q", f.Field)
		}
	}
	whereSQL, err := aggBuildWhere(meta, agg.Where, driver, pb)
	if err != nil {
		return nil, aggregateQueryError(err)
	}

	// GROUP BY
	var groupBySQL string
	if len(agg.GroupBy) > 0 {
		parts := make([]string, len(agg.GroupBy))
		for i, col := range agg.GroupBy {
			parts[i] = aggQuote(meta.TableName) + "." + aggQuote(col)
		}
		groupBySQL = " GROUP BY " + strings.Join(parts, ", ")
	}

	// HAVING — references aggregate aliases. Some drivers (Postgres) do not
	// allow aliases in HAVING; we sidestep that by inlining the aggregate
	// expression.
	var havingSQL string
	if len(agg.Having) > 0 {
		parts := make([]string, 0, len(agg.Having))
		for _, h := range agg.Having {
			af, ok := aliasField[h.Alias]
			if !ok {
				return nil, aggregateQueryErrorf("maniflex: Having references unknown alias %q", h.Alias)
			}
			if !havingOpAllowed(h.Operator) {
				return nil, aggregateQueryErrorf("maniflex: Having operator %q is not supported on aggregates", h.Operator)
			}
			// Re-rendered rather than reusing the SELECT string. A registered
			// expression may carry a bound literal, and a placeholder cannot be
			// textually reused: on SQLite a second "?" consumes the NEXT
			// argument rather than repeating the first, so every value after it
			// would bind one position off. Re-rendering gives this occurrence
			// its own placeholder and its own argument.
			expr, _, err := aggregateSelectSQL(meta, af, pb)
			if err != nil {
				return nil, aggregateQueryError(err)
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", expr, sqlOp(h.Operator), pb.add(h.Value)))
		}
		havingSQL = " HAVING " + strings.Join(parts, " AND ")
	}

	// ORDER BY — alias or group_by column
	var orderBySQL string
	if len(agg.OrderBy) > 0 {
		parts := make([]string, 0, len(agg.OrderBy))
		groupSet := make(map[string]bool, len(agg.GroupBy))
		for _, g := range agg.GroupBy {
			groupSet[g] = true
		}
		for _, s := range agg.OrderBy {
			var col string
			switch {
			case aliases[s.DBName]:
				col = aggQuote(s.DBName)
			case groupSet[s.DBName]:
				col = aggQuote(meta.TableName) + "." + aggQuote(s.DBName)
			default:
				return nil, aggregateQueryErrorf("maniflex: OrderBy %q is not an aggregate alias or GroupBy column", s.DBName)
			}
			dir, derr := aggDirection(s.Direction)
			if derr != nil {
				return nil, aggregateQueryError(derr)
			}
			parts = append(parts, col+" "+dir)
		}
		orderBySQL = " ORDER BY " + strings.Join(parts, ", ")
	}

	// LIMIT
	var limitSQL string
	if agg.Limit > 0 {
		limitSQL = fmt.Sprintf(" LIMIT %d", agg.Limit)
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s%s%s%s%s",
		strings.Join(selectParts, ", "),
		aggQuote(meta.TableName),
		whereSQL, groupBySQL, havingSQL, orderBySQL, limitSQL,
	)

	// rawQuery, not RawQuery: the public one refuses while an ActionScope is in
	// force, and rightly — but this statement is the framework's own, and the
	// scope has already been AND-ed into its WHERE above.
	return c.rawQuery(query, pb.args...)
}

// aggregateSelectSQL returns the SQL expression and resolved alias for one
// AggregateField. Field names either a column on the model or a registered
// AggregateExpr, and is validated unless the op is AggCount with no field
// (COUNT(*)).
//
// It takes the placeholder builder because a registered expression may carry a
// numeric literal, which is bound rather than interpolated.
func aggregateSelectSQL(meta *ModelMeta, f AggregateField, pb *aggPH) (string, string, error) {
	if f.Op == AggCount && f.Field == "" {
		alias := f.As
		if alias == "" {
			alias = "count"
		}
		return "COUNT(*)", alias, nil
	}
	if f.Field == "" {
		return "", "", fmt.Errorf("maniflex: %s requires a Field", f.Op)
	}

	operand, _, err := aggregateOperand(meta, f.Field, pb)
	if err != nil {
		return "", "", err
	}
	alias, err := aggregateAlias(meta, f)
	if err != nil {
		return "", "", err
	}

	switch f.Op {
	case AggCount:
		return fmt.Sprintf("COUNT(%s)", operand), alias, nil
	case AggCountDistinct:
		return fmt.Sprintf("COUNT(DISTINCT %s)", operand), alias, nil
	case AggSum, AggAvg, AggMin, AggMax:
		return fmt.Sprintf("%s(%s)", strings.ToUpper(string(f.Op)), operand), alias, nil
	}
	return "", "", fmt.Errorf("maniflex: unknown aggregate op %q", f.Op)
}

// aggregateAlias returns the result column name for one AggregateField:
// AggregateField.As when set, otherwise the op-and-field default.
//
// Split out from the SQL so the HTTP endpoint can work out which aliases a
// query will produce — which is how it validates an ORDER BY naming one —
// without building any SQL or binding any placeholder.
func aggregateAlias(meta *ModelMeta, f AggregateField) (string, error) {
	if f.As != "" {
		return f.As, nil
	}
	if f.Op == AggCount && f.Field == "" {
		return "count", nil
	}
	if f.Field == "" {
		return "", fmt.Errorf("maniflex: %s requires a Field", f.Op)
	}

	// A registered expression aliases under its own name; a column aliases
	// under its DB name whichever spelling the caller used.
	base := f.Field
	if _, ok := meta.aggExprTree(f.Field); !ok {
		fm := meta.ResolveFilterField(f.Field)
		if fm == nil {
			return "", fmt.Errorf("maniflex: aggregate column %q does not exist on model %q", f.Field, meta.Name)
		}
		base = fm.Tags.DBName
	}

	switch f.Op {
	case AggCount:
		return "count_" + base, nil
	case AggCountDistinct:
		return "count_distinct_" + base, nil
	case AggSum, AggAvg, AggMin, AggMax:
		return string(f.Op) + "_" + base, nil
	}
	return "", fmt.Errorf("maniflex: unknown aggregate op %q", f.Op)
}

// aggregateOperand resolves what an aggregate is computed over: a registered
// expression if the name matches one, otherwise a column on the model. It
// returns the SQL and the stem used to build a default alias.
func aggregateOperand(meta *ModelMeta, name string, pb *aggPH) (string, string, error) {
	if tree, ok := meta.aggExprTree(name); ok {
		sql, err := renderAggExpr(meta, tree, pb)
		if err != nil {
			return "", "", err
		}
		return sql, name, nil
	}
	f := meta.ResolveFilterField(name)
	if f == nil {
		return "", "", fmt.Errorf("maniflex: aggregate column %q does not exist on model %q", name, meta.Name)
	}
	return aggQuote(meta.TableName) + "." + aggQuote(f.Tags.DBName), f.Tags.DBName, nil
}

func havingOpAllowed(op FilterOperator) bool {
	switch op {
	case OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte:
		return true
	}
	return false
}

func sqlOp(op FilterOperator) string {
	switch op {
	case OpEq:
		return "="
	case OpNeq:
		return "<>"
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	}
	return "="
}

// aggBuildWhere composes a minimal WHERE clause for the aggregate path. It
// deliberately does not handle nested-relation filters — that is a future
// enhancement; nested filters are rejected with a clear error instead of
// silently producing bad SQL.
func aggBuildWhere(meta *ModelMeta, filters []*FilterExpr, driver DriverType, pb *aggPH) (string, error) {
	if len(filters) == 0 {
		return "", nil
	}
	for _, f := range filters {
		if f != nil && f.IsNested {
			return "", fmt.Errorf("maniflex: nested-relation filters are not yet supported in Aggregate")
		}
	}

	// Partition by group, exactly as db/sqlcore's filterConds does: Group <= 0
	// (the FilterExpr zero value included) is its own AND clause, and filters
	// sharing a Group >= 1 are OR-ed together. The two builders disagreeing on
	// this is what made a grouped-OR aggregate collapse to the AND intersection
	// — usually zero — while the identical filter listed correctly.
	var ungrouped []*FilterExpr
	grouped := make(map[int][]*FilterExpr)
	var groupOrder []int
	seen := make(map[int]bool)
	for _, f := range filters {
		if f == nil {
			continue
		}
		if f.Group <= 0 {
			ungrouped = append(ungrouped, f)
			continue
		}
		if !seen[f.Group] {
			seen[f.Group] = true
			groupOrder = append(groupOrder, f.Group)
		}
		grouped[f.Group] = append(grouped[f.Group], f)
	}
	sort.Ints(groupOrder) // deterministic SQL

	var parts []string
	for _, f := range ungrouped {
		parts = append(parts, aggCond(meta, f, driver, pb))
	}
	for _, gid := range groupOrder {
		exprs := grouped[gid]
		if len(exprs) == 1 {
			parts = append(parts, aggCond(meta, exprs[0], driver, pb))
			continue
		}
		orParts := make([]string, len(exprs))
		for i, f := range exprs {
			orParts[i] = aggCond(meta, f, driver, pb)
		}
		parts = append(parts, "("+strings.Join(orParts, " OR ")+")")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}

// aggCond renders one aggregate filter as a predicate.
//
// It resolves the field and normalises the value the same way the list path
// does, so the same filter counts what it lists: a json field name resolves to
// its column, a bool column receives a real bool rather than the word "false",
// and a time value is written in the canonical form the adapters store.
//
// Everything it cannot render fails closed to a false predicate rather than
// degrading to something that looks like SQL. That is the half of AG-4 worth
// keeping: the old default branch sent every unhandled operator through sqlOp,
// which answers "=" for anything it does not know, so `amount:between:5,25`
// became `amount = '5,25'` — not an error, just a count of zero. A predicate
// this builder cannot express must match nothing, for the reason db/sqlcore's
// buildCond gives at length: matching everything deletes the filter, and a
// deleted filter on an aggregate is a total computed over rows the caller
// never asked for.
func aggCond(meta *ModelMeta, f *FilterExpr, driver DriverType, pb *aggPH) string {
	fm := meta.ResolveFilterField(f.Field)
	if fm == nil {
		return falsePredicate
	}
	col := aggQuote(meta.TableName) + "." + aggQuote(fm.Tags.DBName)
	val := NormalizeFilterValue(fm, f.Operator, f.Value)

	switch f.Operator {
	case OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte:
		return fmt.Sprintf("%s %s %s", col, sqlOp(f.Operator), pb.add(val))
	case OpIsNull:
		return col + " IS NULL"
	case OpNotNull:
		return col + " IS NOT NULL"
	case OpIn, OpNotIn:
		vals := splitInValue(val)
		if len(vals) == 0 {
			// An empty set has no SQL form: "IN ()" is a syntax error on every
			// driver. Empty IN matches nothing, which is also what an empty set
			// means, so the false predicate is the honest answer either way.
			return falsePredicate
		}
		placeholders := make([]string, len(vals))
		for i, v := range vals {
			placeholders[i] = pb.add(v)
		}
		kw := "IN"
		if f.Operator == OpNotIn {
			kw = "NOT IN"
		}
		return fmt.Sprintf("%s %s (%s)", col, kw, strings.Join(placeholders, ", "))
	case OpBetween:
		bounds := SplitCSV(fmt.Sprint(val))
		if len(bounds) != 2 {
			return falsePredicate
		}
		return fmt.Sprintf("(%s >= %s AND %s <= %s)",
			col, pb.add(bounds[0]), col, pb.add(bounds[1]))
	case OpLike:
		return fmt.Sprintf("%s LIKE %s", col, pb.add(val))
	case OpILike:
		if driver == Postgres {
			return fmt.Sprintf("%s ILIKE %s", col, pb.add(val))
		}
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", col, pb.add(val))
	case OpContains, OpStartsWith, OpEndsWith:
		// Escaped pattern + an explicit ESCAPE, so % and _ in the value match
		// themselves and both drivers agree on what escapes what.
		pattern := LikePattern(f.Operator, val)
		if driver == Postgres {
			return fmt.Sprintf("%s ILIKE %s ESCAPE '\\'", col, pb.add(pattern))
		}
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s) ESCAPE '\\'", col, pb.add(pattern))
	}
	return falsePredicate
}

// falsePredicate is the condition a builder emits when it cannot render the
// filter it was given. Spelled once so the aggregate path and the adapters
// answer an impossible filter the same way.
const falsePredicate = "1=0"

func splitInValue(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case string:
		parts := strings.Split(x, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// aggPH is a tiny placeholder builder local to this file so we don't reach
// into the sqlcore package (which would create an import cycle).
type aggPH struct {
	driver DriverType
	args   []any
}

func newAggPH(driver DriverType) *aggPH { return &aggPH{driver: driver} }

func (p *aggPH) add(v any) string {
	p.args = append(p.args, v)
	if p.driver == Postgres {
		return "$" + strconv.Itoa(len(p.args))
	}
	return "?"
}

// aggDirection maps a SortExpr.Direction onto the only two words allowed to
// reach the SQL, rather than upper-casing whatever the caller supplied.
//
// The column side of ORDER BY is already checked against the aggregate aliases
// and GroupBy columns, but the direction was concatenated raw. That is safe from
// an HTTP request — the endpoint constrains it to asc/desc — and unsafe from
// ctx.Aggregate, which is a developer API that may well be handed a value that
// came from a user (audit MS-L3). Same class as SEC-1/SEC-2, reachable only
// through the Go API.
//
// An empty Direction keeps its previous meaning of ASC, which is what the zero
// value has always produced.
func aggDirection(d SortDir) (string, error) {
	switch strings.ToLower(strings.TrimSpace(string(d))) {
	case "", "asc":
		return "ASC", nil
	case "desc":
		return "DESC", nil
	}
	return "", fmt.Errorf("maniflex: OrderBy direction %q is not valid (use asc or desc)", d)
}

// aggQuote wraps an identifier in double quotes and escapes any embedded
// quote. Mirrors sqlcore.Quote without the package dependency.
func aggQuote(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
