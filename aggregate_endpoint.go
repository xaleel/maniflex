package maniflex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// aggregateRequest is the JSON spec accepted by the auto-generated
// GET /:model/aggregate?aggregate=<url-encoded JSON> endpoint
// (ModelConfig.AggregateEnabled). It mirrors AggregateQuery but with
// JSON-friendly names and is translated into an AggregateQuery only after every
// referenced field is checked against the model's filterable/sortable allow-list
// — so the public endpoint can never aggregate over a column the model has not
// opted into exposing.
type aggregateRequest struct {
	Select  []aggregateSelectJSON `json:"select"`
	GroupBy []string              `json:"group_by"`
	Where   []aggregateWhereJSON  `json:"where"`
	Having  []aggregateHavingJSON `json:"having"`
	OrderBy []aggregateSortJSON   `json:"order_by"`
	Limit   int                   `json:"limit"`
}

type aggregateSelectJSON struct {
	Op    string `json:"op"`
	Field string `json:"field"`
	As    string `json:"as"`
}

type aggregateWhereJSON struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

type aggregateHavingJSON struct {
	Alias    string `json:"alias"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

type aggregateSortJSON struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// aggregateOps is the set of aggregate functions the HTTP endpoint exposes.
var aggregateOps = map[string]AggregateOp{
	"count":          AggCount,
	"count_distinct": AggCountDistinct,
	"sum":            AggSum,
	"avg":            AggAvg,
	"min":            AggMin,
	"max":            AggMax,
}

// aggregateWhereOps is the set of filter operators the aggregate WHERE clause
// supports. It mirrors what aggBuildWhere can render; notably "between" is
// excluded because the aggregate WHERE builder does not expand it.
var aggregateWhereOps = map[FilterOperator]bool{
	OpEq: true, OpNeq: true, OpGt: true, OpGte: true, OpLt: true, OpLte: true,
	OpLike: true, OpILike: true, OpIn: true, OpNotIn: true,
	OpIsNull: true, OpNotNull: true,
	OpContains: true, OpStartsWith: true, OpEndsWith: true,
}

// buildAggregateQuery parses the ?aggregate= spec into an AggregateQuery,
// resolving every field reference to its DB column name and rejecting any field
// that is not in the model's filterable/sortable allow-list. The returned error
// carries a client-facing message suitable for a 400 response.
func buildAggregateQuery(spec []byte, model *ModelMeta) (AggregateQuery, error) {
	if len(spec) == 0 {
		return AggregateQuery{}, fmt.Errorf("aggregate query must not be empty")
	}
	var req aggregateRequest
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return AggregateQuery{}, fmt.Errorf("malformed aggregate query: %s", err.Error())
	}
	if len(req.Select) == 0 {
		return AggregateQuery{}, fmt.Errorf("aggregate query must contain at least one select entry")
	}

	allowed := aggregateAllowedColumns(model)

	q := AggregateQuery{Limit: req.Limit}

	// SELECT — translate op + field; COUNT(*) (op "count" with no field) needs no
	// column. selectAliases tracks resolved aliases so ORDER BY can reference them.
	selectAliases := make(map[string]bool, len(req.Select))
	for _, sf := range req.Select {
		op, ok := aggregateOps[sf.Op]
		if !ok {
			return AggregateQuery{}, fmt.Errorf("unknown aggregate op %q", sf.Op)
		}
		field := sf.Field
		if field != "" {
			dbName, err := resolveAggregateColumn(field, model, allowed)
			if err != nil {
				return AggregateQuery{}, err
			}
			field = dbName
		}
		af := AggregateField{Op: op, Field: field, As: sf.As}
		// Resolve the alias the same way aggregateExpr will, so ORDER BY field
		// names can be matched against it below.
		if _, alias, err := aggregateExpr(model, af); err == nil {
			selectAliases[alias] = true
		}
		q.Select = append(q.Select, af)
	}

	// GROUP BY — every column must be in the allow-list. groupCols tracks the
	// resolved DB names so ORDER BY can reference them.
	groupCols := make(map[string]bool, len(req.GroupBy))
	for _, g := range req.GroupBy {
		dbName, err := resolveAggregateColumn(g, model, allowed)
		if err != nil {
			return AggregateQuery{}, err
		}
		groupCols[dbName] = true
		q.GroupBy = append(q.GroupBy, dbName)
	}

	// WHERE — flat conditions only; field must be in the allow-list and the
	// operator must be one the aggregate WHERE builder supports.
	for _, w := range req.Where {
		op := FilterOperator(w.Operator)
		if !aggregateWhereOps[op] {
			return AggregateQuery{}, fmt.Errorf("unsupported aggregate filter operator %q", w.Operator)
		}
		dbName, err := resolveAggregateColumn(w.Field, model, allowed)
		if err != nil {
			return AggregateQuery{}, err
		}
		q.Where = append(q.Where, &FilterExpr{Field: dbName, Operator: op, Value: w.Value, Group: -1})
	}

	// HAVING — references a select alias; operator restricted to the comparison
	// set. Alias existence is validated by ctx.Aggregate.
	for _, h := range req.Having {
		op := FilterOperator(h.Operator)
		if !havingOpAllowed(op) {
			return AggregateQuery{}, fmt.Errorf("unsupported having operator %q", h.Operator)
		}
		q.Having = append(q.Having, HavingClause{Alias: h.Alias, Operator: op, Value: h.Value})
	}

	// ORDER BY — a select alias (kept verbatim) or a group_by column (resolved to
	// its DB name). ctx.Aggregate performs the final alias-or-group check.
	for _, o := range req.OrderBy {
		dir, err := aggregateSortDir(o.Direction)
		if err != nil {
			return AggregateQuery{}, err
		}
		name := o.Field
		if !selectAliases[name] {
			if dbName, ok := aggregateColumnDBName(name, model); ok {
				name = dbName
			}
		}
		q.OrderBy = append(q.OrderBy, SortExpr{DBName: name, Direction: dir})
	}

	return q, nil
}

// aggregateDB is the DB-step handler for the aggregate endpoint. It folds any
// request ?filter= conditions and middleware-injected tenancy force-filters
// (both live in ctx.Query.Filters) into the WHERE clause ahead of the body's own
// conditions, so row-level isolation applies to aggregates exactly as it does to
// list reads, then runs the query through ctx.Aggregate.
func (s *defaultSteps) aggregateDB(ctx *ServerContext, next func() error) error {
	if ctx.aggQuery == nil {
		ctx.Abort(http.StatusInternalServerError, "AGGREGATE_NO_QUERY",
			"aggregate query was not parsed")
		return nil
	}
	q := *ctx.aggQuery
	if ctx.Query != nil && len(ctx.Query.Filters) > 0 {
		merged := make([]*FilterExpr, 0, len(ctx.Query.Filters)+len(q.Where))
		merged = append(merged, ctx.Query.Filters...)
		merged = append(merged, q.Where...)
		q.Where = merged
	}

	rows, err := ctx.Aggregate(ctx.Model.Name, q)
	if err != nil {
		ctx.Abort(http.StatusBadRequest, "AGGREGATE_ERROR", err.Error())
		return nil
	}
	if rows == nil {
		rows = []Row{}
	}
	ctx.DBResult = rows
	return next()
}

// aggregateAllowedColumns is the set of DB column names the aggregate endpoint
// may reference: the union of filterable and sortable fields.
func aggregateAllowedColumns(model *ModelMeta) map[string]bool {
	out := make(map[string]bool)
	for i := range model.Fields {
		f := &model.Fields[i]
		if f.Tags.Filterable || f.Tags.Sortable {
			out[f.Tags.DBName] = true
		}
	}
	return out
}

// aggregateColumnDBName resolves a JSON or DB field name to its DB column name
// without an allow-list check. Used for ORDER BY, where a name may instead be an
// aggregate alias that has no backing column.
func aggregateColumnDBName(name string, model *ModelMeta) (string, bool) {
	if f := model.FieldByJSONName(name); f != nil {
		return f.Tags.DBName, true
	}
	if f := model.FieldByDBName(name); f != nil {
		return f.Tags.DBName, true
	}
	return "", false
}

// resolveAggregateColumn resolves a JSON or DB field name to its DB column name
// and requires it to be in the filterable/sortable allow-list.
func resolveAggregateColumn(name string, model *ModelMeta, allowed map[string]bool) (string, error) {
	dbName, ok := aggregateColumnDBName(name, model)
	if !ok {
		return "", fmt.Errorf("field %q not found on model %s", name, model.Name)
	}
	if !allowed[dbName] {
		return "", fmt.Errorf("field %q on model %s is not filterable or sortable (add mfx:\"filterable\" or mfx:\"sortable\")", name, model.Name)
	}
	return dbName, nil
}

// aggregateSortDir maps a JSON direction string to a SortDir, defaulting to
// ascending when empty.
func aggregateSortDir(dir string) (SortDir, error) {
	switch dir {
	case "", "asc":
		return SortAsc, nil
	case "desc":
		return SortDesc, nil
	}
	return "", fmt.Errorf("invalid sort direction %q (want \"asc\" or \"desc\")", dir)
}
