package maniflex

import (
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
		return nil, fmt.Errorf("maniflex: AggregateQuery.Select must contain at least one field")
	}

	driver := c.DriverType()
	pb := newAggPH(driver)

	// SELECT
	selectParts := make([]string, 0, len(agg.Select)+len(agg.GroupBy))
	aliases := make(map[string]bool, len(agg.Select))
	aliasExpr := make(map[string]string, len(agg.Select))
	for _, f := range agg.Select {
		expr, alias, err := aggregateExpr(meta, f)
		if err != nil {
			return nil, err
		}
		if aliases[alias] {
			return nil, fmt.Errorf("maniflex: duplicate aggregate alias %q", alias)
		}
		aliases[alias] = true
		aliasExpr[alias] = expr
		selectParts = append(selectParts, expr+" AS "+aggQuote(alias))
	}
	// Always include the GROUP BY columns in the SELECT so the caller sees
	// which group each row belongs to.
	for _, col := range agg.GroupBy {
		if meta.FieldByDBName(col) == nil {
			return nil, fmt.Errorf("maniflex: GroupBy column %q does not exist on model %q", col, modelName)
		}
		selectParts = append(selectParts, aggQuote(meta.TableName)+"."+aggQuote(col))
	}

	// WHERE — validate column names against the model so an unknown column is
	// a clean error rather than a SQL fault.
	for _, f := range agg.Where {
		if !f.IsNested && meta.FieldByDBName(f.Field) == nil {
			return nil, fmt.Errorf("maniflex: Where filter references unknown column %q", f.Field)
		}
	}
	whereSQL, err := aggBuildWhere(meta, agg.Where, driver, pb)
	if err != nil {
		return nil, err
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
			expr, ok := aliasExpr[h.Alias]
			if !ok {
				return nil, fmt.Errorf("maniflex: Having references unknown alias %q", h.Alias)
			}
			if !havingOpAllowed(h.Operator) {
				return nil, fmt.Errorf("maniflex: Having operator %q is not supported on aggregates", h.Operator)
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
				return nil, fmt.Errorf("maniflex: OrderBy %q is not an aggregate alias or GroupBy column", s.DBName)
			}
			dir, derr := aggDirection(s.Direction)
			if derr != nil {
				return nil, derr
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

// aggregateExpr returns the SQL expression and resolved alias for one
// AggregateField. Field is validated against the model unless the op is
// AggCount with no field (COUNT(*)).
func aggregateExpr(meta *ModelMeta, f AggregateField) (string, string, error) {
	switch f.Op {
	case AggCount:
		if f.Field == "" {
			alias := f.As
			if alias == "" {
				alias = "count"
			}
			return "COUNT(*)", alias, nil
		}
		if meta.FieldByDBName(f.Field) == nil {
			return "", "", fmt.Errorf("maniflex: aggregate column %q does not exist on model %q", f.Field, meta.Name)
		}
		alias := f.As
		if alias == "" {
			alias = "count_" + f.Field
		}
		return fmt.Sprintf("COUNT(%s.%s)",
			aggQuote(meta.TableName), aggQuote(f.Field)), alias, nil
	case AggCountDistinct:
		if f.Field == "" {
			return "", "", fmt.Errorf("maniflex: AggCountDistinct requires a Field")
		}
		if meta.FieldByDBName(f.Field) == nil {
			return "", "", fmt.Errorf("maniflex: aggregate column %q does not exist on model %q", f.Field, meta.Name)
		}
		alias := f.As
		if alias == "" {
			alias = "count_distinct_" + f.Field
		}
		return fmt.Sprintf("COUNT(DISTINCT %s.%s)",
			aggQuote(meta.TableName), aggQuote(f.Field)), alias, nil
	case AggSum, AggAvg, AggMin, AggMax:
		if f.Field == "" {
			return "", "", fmt.Errorf("maniflex: %s requires a Field", f.Op)
		}
		if meta.FieldByDBName(f.Field) == nil {
			return "", "", fmt.Errorf("maniflex: aggregate column %q does not exist on model %q", f.Field, meta.Name)
		}
		alias := f.As
		if alias == "" {
			alias = string(f.Op) + "_" + f.Field
		}
		return fmt.Sprintf("%s(%s.%s)",
			strings.ToUpper(string(f.Op)),
			aggQuote(meta.TableName), aggQuote(f.Field)), alias, nil
	}
	return "", "", fmt.Errorf("maniflex: unknown aggregate op %q", f.Op)
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
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		if f.IsNested {
			return "", fmt.Errorf("maniflex: nested-relation filters are not yet supported in Aggregate")
		}
		col := aggQuote(meta.TableName) + "." + aggQuote(f.Field)
		switch f.Operator {
		case OpIsNull:
			parts = append(parts, col+" IS NULL")
		case OpNotNull:
			parts = append(parts, col+" IS NOT NULL")
		case OpIn, OpNotIn:
			vals := splitInValue(f.Value)
			if len(vals) == 0 {
				return "", fmt.Errorf("maniflex: filter %s on %q has no values", f.Operator, f.Field)
			}
			placeholders := make([]string, len(vals))
			for i, v := range vals {
				placeholders[i] = pb.add(v)
			}
			kw := "IN"
			if f.Operator == OpNotIn {
				kw = "NOT IN"
			}
			parts = append(parts, fmt.Sprintf("%s %s (%s)", col, kw, strings.Join(placeholders, ", ")))
		case OpLike:
			parts = append(parts, fmt.Sprintf("%s LIKE %s", col, pb.add(f.Value)))
		case OpILike:
			if driver == Postgres {
				parts = append(parts, fmt.Sprintf("%s ILIKE %s", col, pb.add(f.Value)))
			} else {
				parts = append(parts, fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", col, pb.add(f.Value)))
			}
		case OpContains, OpStartsWith, OpEndsWith:
			// Escaped pattern + an explicit ESCAPE, so % and _ in the value match
			// themselves and both drivers agree on what escapes what.
			pattern := LikePattern(f.Operator, f.Value)
			if driver == Postgres {
				parts = append(parts, fmt.Sprintf("%s ILIKE %s ESCAPE '\\'", col, pb.add(pattern)))
			} else {
				parts = append(parts, fmt.Sprintf("LOWER(%s) LIKE LOWER(%s) ESCAPE '\\'", col, pb.add(pattern)))
			}
		default:
			parts = append(parts, fmt.Sprintf("%s %s %s", col, sqlOp(f.Operator), pb.add(f.Value)))
		}
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}

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
