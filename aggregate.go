package maniflex

import (
	"encoding/json"
	"fmt"
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
	numericAliases := make(map[string]bool, len(agg.Select))
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
		if aggregateYieldsNumber(meta, f) {
			numericAliases[alias] = true
		}
		selectParts = append(selectParts, expr+" AS "+Quote(alias))
	}
	// Always include the GROUP BY columns in the SELECT so the caller sees
	// which group each row belongs to.
	for _, col := range agg.GroupBy {
		if meta.FieldByDBName(col) == nil {
			return nil, aggregateQueryErrorf("maniflex: GroupBy column %q does not exist on model %q", col, modelName)
		}
		selectParts = append(selectParts, Quote(meta.TableName)+"."+Quote(col))
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
			parts[i] = Quote(meta.TableName) + "." + Quote(col)
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
				col = Quote(s.DBName)
			case groupSet[s.DBName]:
				col = Quote(meta.TableName) + "." + Quote(s.DBName)
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
		Quote(meta.TableName),
		whereSQL, groupBySQL, havingSQL, orderBySQL, limitSQL,
	)

	// rawQuery, not RawQuery: the public one refuses while an ActionScope is in
	// force, and rightly — but this statement is the framework's own, and the
	// scope has already been AND-ed into its WHERE above.
	rows, err := c.rawQuery(query, pb.args...)
	if err != nil {
		return nil, err
	}
	// A numeric result must be a JSON number whichever driver answered. Only
	// here, not in rawQuery: RawQuery is the escape hatch and returns what the
	// driver gave.
	normalizeAggregateNumbers(rows, numericAliases)
	return rows, nil
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

// aggregateYieldsNumber reports whether an aggregate's result is always a
// number, so a driver that hands it back as text can be normalised without
// touching a value that is legitimately a string.
//
// count and count_distinct are integers, and sum and avg are numeric by
// definition. min and max return the operand's own type — MIN over a text
// column is text, and rewriting a value like "00123" into a number there would
// corrupt it — so they qualify only when the operand is numeric. A registered
// expression always qualifies: its columns are checked numeric at registration.
func aggregateYieldsNumber(meta *ModelMeta, f AggregateField) bool {
	switch f.Op {
	case AggCount, AggCountDistinct, AggSum, AggAvg:
		return true
	case AggMin, AggMax:
		if _, ok := meta.aggExprTree(f.Field); ok {
			return true
		}
		fm := meta.ResolveFilterField(f.Field)
		return fm != nil && isNumericKind(fm.Type)
	}
	return false
}

// normalizeAggregateNumbers rewrites text-valued numeric aggregate results into
// json.Number, so the same query answers with a JSON number on every driver.
//
// lib/pq scans a Postgres NUMERIC — which is what SUM over a BIGINT, and AVG
// over anything, come back as — into a []byte that the row scanner turns into a
// Go string. SQLite returns an int64 or a float64. The same aggregate therefore
// produced {"sum_amount": "150"} against one driver and {"sum_amount": 150}
// against the other, and every consumer had to accept both.
//
// json.Number rather than float64: it marshals as the exact digits the database
// produced, so a total wider than float64's 53-bit mantissa is not silently
// rounded, and a large round number is not re-rendered in exponent form.
//
// Only aliases in `numeric` are touched, so a group_by column and a MIN over a
// text column keep their strings.
func normalizeAggregateNumbers(rows []Row, numeric map[string]bool) {
	for _, row := range rows {
		for alias := range numeric {
			v, ok := row[alias]
			if !ok {
				continue
			}
			var s string
			switch t := v.(type) {
			case string:
				s = t
			case []byte:
				s = string(t)
			default:
				continue // already a number, or NULL
			}
			if isJSONNumber(s) {
				row[alias] = json.Number(s)
			}
		}
	}
}

// isJSONNumber reports whether s is a JSON number literal, matching the grammar
// encoding/json enforces when marshalling a json.Number.
//
// The check is not paranoia. A Postgres NUMERIC also admits 'NaN' and
// 'Infinity', and neither is a JSON number — handing one to the encoder as a
// bare token fails the marshal and takes the whole response down with it, so a
// value this rejects stays the string it already was.
func isJSONNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	// Integer part: "0" or [1-9][0-9]*. Leading zeros are invalid JSON.
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return false
	}
	// Optional fraction, which must have at least one digit.
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	// Optional exponent, likewise.
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

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
	return Quote(meta.TableName) + "." + Quote(f.Tags.DBName), f.Tags.DBName, nil
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

// aggBuildWhere composes the WHERE clause for the aggregate path.
//
// The rendering is BuildFilterSQL, shared with every other query the framework
// builds (audit AG-5). This function is what remains specific to the aggregate:
// the leading " WHERE ", and the one filter kind the aggregate may not accept.
//
// Nested-relation filters render as "relation"."column", which needs a JOIN the
// aggregate FROM clause does not emit. They are refused here rather than passed
// to the builder, so the failure is an error naming the limitation instead of
// SQL referencing a table the query never joined.
func aggBuildWhere(meta *ModelMeta, filters []*FilterExpr, driver DriverType, pb *aggPH) (string, error) {
	for _, f := range filters {
		if f != nil && f.IsNested {
			return "", fmt.Errorf("maniflex: nested-relation filters are not yet supported in Aggregate")
		}
	}
	conds := BuildFilterSQL(meta, filters, driver, pb)
	if conds == "" {
		return "", nil
	}
	return " WHERE " + conds, nil
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

// Add is the PlaceholderBinder spelling, so this builder can be handed to
// BuildFilterSQL. The lower-case add stays because ~19 call sites in this
// package use it and the two must not drift apart.
func (p *aggPH) Add(v any) string { return p.add(v) }

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
