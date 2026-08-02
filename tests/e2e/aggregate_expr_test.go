package e2e

// AG-2 — expression aggregates end to end. The point of the feature is that a
// total the schema does not store becomes one SQL query instead of a Go loop
// over paged rows, so these tests check the number, not just the SQL.

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ExprLine struct {
	maniflex.BaseModel
	Sku   string `json:"sku"   db:"sku"   mfx:"required,filterable"`
	Price int    `json:"price" db:"price" mfx:"filterable,sortable"`
	Cost  int    `json:"cost"  db:"cost"  mfx:"filterable,sortable"`
	Count int    `json:"count" db:"count" mfx:"filterable,sortable"`
}

// exprLines is the fixture, with the two figures worked out by hand:
//
//	revenue = Σ price×count  = 10*3 + 20*2 + 5*10 = 30 + 40 + 50 = 120
//	margin  = Σ (price−cost)×count = 4*3 + 12*2 + 2*10 = 12 + 24 + 20 = 56
var exprLines = []map[string]any{
	{"sku": "a", "price": 10, "cost": 6, "count": 3},
	{"sku": "b", "price": 20, "cost": 8, "count": 2},
	{"sku": "c", "price": 5, "cost": 3, "count": 10},
}

const (
	wantRevenue = 120.0
	wantMargin  = 56.0
)

func exprServer(t *testing.T, exposed bool) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			ExprLine{},
			maniflex.ModelConfig{AggregateEnabled: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.MustRegisterAggregateExpr(maniflex.AggregateExpr{
				Model:   "ExprLine",
				Name:    "revenue",
				Expr:    maniflex.Mul(maniflex.Col("price"), maniflex.Col("count")),
				Exposed: exposed,
			})
			s.MustRegisterAggregateExpr(maniflex.AggregateExpr{
				Model: "ExprLine",
				Name:  "margin",
				Expr: maniflex.Mul(
					maniflex.Sub(maniflex.Col("price"), maniflex.Col("cost")),
					maniflex.Col("count"),
				),
				Exposed: exposed,
			})
			s.Action(maniflex.ActionConfig{
				Method:      "GET",
				Path:        "/expr-totals",
				AllowPublic: true,
				Handler: func(ctx *maniflex.ServerContext) error {
					rows, err := ctx.Aggregate("ExprLine", maniflex.AggregateQuery{
						Select: []maniflex.AggregateField{
							{Op: maniflex.AggSum, Field: "revenue", As: "revenue"},
							{Op: maniflex.AggSum, Field: "margin", As: "margin"},
						},
					})
					if err != nil {
						ctx.Abort(http.StatusInternalServerError, "AGG", err.Error())
						return nil
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"rows": rows},
					}
					return nil
				},
			})
		},
	})
	for _, row := range exprLines {
		srv.POST("/expr_lines", row).AssertStatus(http.StatusCreated)
	}
	return srv
}

func numOf(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case string: // Postgres returns SUM(NUMERIC) as a JSON string (AG-3)
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			t.Fatalf("cannot read %q as a number: %v", n, err)
		}
		return f
	}
	t.Fatalf("unexpected numeric type %T", v)
	return 0
}

// The typed API is where the recorded need was: be2's dashboard computed these
// in Go over paged FindMany rows.
func TestAggregateExpr_TypedAPI(t *testing.T) {
	t.Parallel()
	srv := exprServer(t, false)

	resp := srv.GET("/expr-totals")
	resp.AssertStatus(http.StatusOK)
	rows, _ := resp.Data()["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d: %s", len(rows), resp.Body)
	}
	row, _ := rows[0].(map[string]any)
	if got := numOf(t, row["revenue"]); got != wantRevenue {
		t.Fatalf("revenue = %v, want %v", got, wantRevenue)
	}
	if got := numOf(t, row["margin"]); got != wantMargin {
		t.Fatalf("margin = %v, want %v", got, wantMargin)
	}
}

// Exposed makes the same expression reachable from the generated endpoint,
// through the ordinary select shape — no new wire format.
func TestAggregateExpr_HTTPWhenExposed(t *testing.T) {
	t.Parallel()
	srv := exprServer(t, true)

	spec := url.QueryEscape(`{"select":[{"op":"sum","field":"revenue","as":"revenue"}]}`)
	resp := srv.GET("/expr_lines/aggregate?aggregate=" + spec)
	resp.AssertStatus(http.StatusOK)

	rows := resp.DataList()
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d: %s", len(rows), resp.Body)
	}
	row, _ := rows[0].(map[string]any)
	if got := numOf(t, row["revenue"]); got != wantRevenue {
		t.Fatalf("revenue = %v, want %v", got, wantRevenue)
	}
}

// Default-closed: registering an expression does not publish it.
func TestAggregateExpr_HTTPRefusedWhenNotExposed(t *testing.T) {
	t.Parallel()
	srv := exprServer(t, false)

	spec := url.QueryEscape(`{"select":[{"op":"sum","field":"revenue","as":"revenue"}]}`)
	resp := srv.GET("/expr_lines/aggregate?aggregate=" + spec)
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400\n%s", resp.Status, resp.Body)
	}
}

// An expression composes with the rest of the query — filters narrow it and
// group_by splits it — because it is just another thing to aggregate.
func TestAggregateExpr_WithFilterAndGroupBy(t *testing.T) {
	t.Parallel()
	srv := exprServer(t, true)

	spec := url.QueryEscape(`{"select":[{"op":"sum","field":"revenue","as":"revenue"}],"group_by":["sku"]}`)
	resp := srv.GET("/expr_lines/aggregate?aggregate=" + spec + "&filter=sku:in:a,b")
	resp.AssertStatus(http.StatusOK)

	rows := resp.DataList()
	if len(rows) != 2 {
		t.Fatalf("want two groups, got %d: %s", len(rows), resp.Body)
	}
	total := 0.0
	for _, r := range rows {
		row, _ := r.(map[string]any)
		total += numOf(t, row["revenue"])
	}
	// a and b only: 10*3 + 20*2 = 70.
	if total != 70 {
		t.Fatalf("filtered revenue = %v, want 70", total)
	}
}

// HAVING inlines the SELECT expression rather than naming its alias, because
// Postgres will not take an alias there. That makes a bound literal a hazard: a
// placeholder cannot be textually reused, since on SQLite a second "?" consumes
// the NEXT argument rather than repeating the first, so every value after it
// binds one position off. The builder re-renders instead, and this is what
// would catch it regressing — the threshold has to land in HAVING, not in the
// expression.
//
// revenue2 is price×2, so the per-sku totals are 20, 40 and 10; only two clear
// a threshold of 15.
func TestAggregateExpr_LiteralWithHaving(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			ExprLine{},
			maniflex.ModelConfig{AggregateEnabled: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.MustRegisterAggregateExpr(maniflex.AggregateExpr{
				Model:   "ExprLine",
				Name:    "revenue2",
				Expr:    maniflex.Mul(maniflex.Col("price"), maniflex.Lit(2)),
				Exposed: true,
			})
		},
	})
	for _, row := range exprLines {
		srv.POST("/expr_lines", row).AssertStatus(http.StatusCreated)
	}

	spec := url.QueryEscape(`{"select":[{"op":"sum","field":"revenue2","as":"r"}],` +
		`"group_by":["sku"],"having":[{"alias":"r","operator":"gt","value":15}]}`)
	resp := srv.GET("/expr_lines/aggregate?aggregate=" + spec)
	resp.AssertStatus(http.StatusOK)

	rows := resp.DataList()
	if len(rows) != 2 {
		t.Fatalf("want two groups over the threshold, got %d: %s", len(rows), resp.Body)
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if got := numOf(t, row["r"]); got <= 15 {
			t.Fatalf("group %v is below the HAVING threshold: %v\n%s", row["sku"], got, resp.Body)
		}
	}
}

// GROUP BY and ORDER BY take columns; an expression there is an unknown field
// rather than a half-supported one.
func TestAggregateExpr_NotAllowedInGroupBy(t *testing.T) {
	t.Parallel()
	srv := exprServer(t, true)

	spec := url.QueryEscape(`{"select":[{"op":"count","as":"n"}],"group_by":["revenue"]}`)
	resp := srv.GET("/expr_lines/aggregate?aggregate=" + spec)
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400\n%s", resp.Status, resp.Body)
	}
}
