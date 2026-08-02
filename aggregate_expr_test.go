package maniflex

// AG-2 — expression aggregates. An app registers a named SQL expression on a
// model and clients aggregate over it by name, so sum(price*count) is one query
// rather than a Go loop over paged rows.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func exprModel() *ModelMeta {
	return &ModelMeta{
		Name:      "Item",
		TableName: "items",
		Fields: []FieldMeta{
			{Name: "Price", Type: reflect.TypeOf(0),
				Tags: FieldTags{DBName: "price", JSONName: "price", Filterable: true}},
			{Name: "Cost", Type: reflect.TypeOf(0),
				Tags: FieldTags{DBName: "cost", JSONName: "cost", Sortable: true}},
			{Name: "Count", Type: reflect.TypeOf(0),
				Tags: FieldTags{DBName: "count", JSONName: "itemCount", Filterable: true}},
			{Name: "Ratio", Type: reflect.TypeOf(0.0),
				Tags: FieldTags{DBName: "ratio", JSONName: "ratio"}},
			{Name: "Title", Type: reflect.TypeOf(""),
				Tags: FieldTags{DBName: "title", JSONName: "title"}},
		},
	}
}

func renderExprSQL(t *testing.T, driver DriverType, e Expr) (string, []any) {
	t.Helper()
	pb := newAggPH(driver)
	sql, err := renderAggExpr(exprModel(), e, pb)
	if err != nil {
		t.Fatalf("renderAggExpr: %v", err)
	}
	return sql, pb.args
}

// ── rendering ─────────────────────────────────────────────────────────────────

func TestAggExpr_Mul(t *testing.T) {
	sql, args := renderExprSQL(t, SQLite, Mul(Col("price"), Col("count")))
	want := `("items"."price" * "items"."count")`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	if len(args) != 0 {
		t.Fatalf("a column-only expression binds nothing, got %v", args)
	}
}

func TestAggExpr_AddSub(t *testing.T) {
	sql, _ := renderExprSQL(t, SQLite, Add(Col("price"), Col("cost")))
	if want := `("items"."price" + "items"."cost")`; sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	sql, _ = renderExprSQL(t, SQLite, Sub(Col("price"), Col("cost")))
	if want := `("items"."price" - "items"."cost")`; sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

// Division wraps its divisor in NULLIF so the drivers agree: SQLite answers
// NULL for x/0 and Postgres raises, which would make the same report return a
// row locally and 500 in production.
func TestAggExpr_DivWrapsDivisorInNULLIF(t *testing.T) {
	sql, _ := renderExprSQL(t, SQLite, Div(Col("price"), Col("count")))
	want := `("items"."price" / NULLIF("items"."count", 0))`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

func TestAggExpr_Nested(t *testing.T) {
	sql, _ := renderExprSQL(t, SQLite,
		Mul(Sub(Col("price"), Col("cost")), Col("count")))
	want := `(("items"."price" - "items"."cost") * "items"."count")`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
}

// A literal is a bound parameter, never interpolated — the property that makes
// the whole expression type safe to hand a public endpoint.
func TestAggExpr_LiteralIsBound(t *testing.T) {
	sql, args := renderExprSQL(t, SQLite, Mul(Col("price"), Lit(0.9)))
	want := `("items"."price" * ?)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	if len(args) != 1 || args[0] != 0.9 {
		t.Fatalf("literal must be bound, got %v", args)
	}
}

func TestAggExpr_PostgresPlaceholders(t *testing.T) {
	sql, args := renderExprSQL(t, Postgres, Add(Mul(Col("price"), Lit(2)), Lit(1)))
	want := `(("items"."price" * $1) + $2)`
	if sql != want {
		t.Fatalf("\n got  %q\n want %q", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 bound literals, got %v", args)
	}
}

// Col takes either spelling, like every other field reference.
func TestAggExpr_ColAcceptsJSONName(t *testing.T) {
	sql, _ := renderExprSQL(t, SQLite, Mul(Col("price"), Col("itemCount")))
	want := `("items"."price" * "items"."count")`
	if sql != want {
		t.Fatalf("json column name\n got  %q\n want %q", sql, want)
	}
}

// ── registration validation ───────────────────────────────────────────────────

func compileFor(t *testing.T, e AggregateExpr) error {
	t.Helper()
	_, err := compileAggregateExpr(exprModel(), e)
	return err
}

func TestAggExpr_CompileAcceptsValid(t *testing.T) {
	if err := compileFor(t, AggregateExpr{
		Name: "revenue", Expr: Mul(Col("price"), Col("count")),
	}); err != nil {
		t.Fatalf("valid expression rejected: %v", err)
	}
}

func TestAggExpr_CompileRejects(t *testing.T) {
	cases := []struct {
		name string
		expr AggregateExpr
		want string
	}{
		{"empty name", AggregateExpr{Expr: Col("price")}, "name"},
		{"nil expression", AggregateExpr{Name: "x"}, "expression"},
		{"unknown column", AggregateExpr{Name: "x", Expr: Col("nope")}, "nope"},
		{"non-numeric column", AggregateExpr{Name: "x", Expr: Mul(Col("title"), Col("price"))}, "numeric"},
		{"collides with field db name", AggregateExpr{Name: "price", Expr: Col("price")}, "price"},
		{"collides with field json name", AggregateExpr{Name: "itemCount", Expr: Col("price")}, "itemCount"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compileFor(t, tc.expr)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A pathological expression is a startup error, so nothing has to be bounded at
// request time.
func TestAggExpr_CompileRejectsDeepExpression(t *testing.T) {
	e := Expr(Col("price"))
	for i := 0; i < maxAggExprDepth+2; i++ {
		e = Mul(e, Col("price"))
	}
	err := compileFor(t, AggregateExpr{Name: "deep", Expr: e})
	if err == nil || !strings.Contains(err.Error(), "deep") && !strings.Contains(err.Error(), "nested") {
		t.Fatalf("want a depth error, got %v", err)
	}
}

// ── server registration ───────────────────────────────────────────────────────

func TestAggExpr_RegisterRejectsUnknownModel(t *testing.T) {
	s := New(Config{})
	err := s.RegisterAggregateExpr(AggregateExpr{
		Model: "Nope", Name: "x", Expr: Col("price"),
	})
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("want an unknown-model error, got %v", err)
	}
}

func TestAggExpr_RegisterRejectsDuplicateName(t *testing.T) {
	type Item struct {
		BaseModel
		Price int `json:"price" db:"price" mfx:"filterable"`
		Count int `json:"count" db:"count" mfx:"filterable"`
	}
	s := New(Config{})
	_ = s.Register(Item{})
	e := AggregateExpr{Model: "Item", Name: "revenue", Expr: Mul(Col("price"), Col("count"))}
	if err := s.RegisterAggregateExpr(e); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := s.RegisterAggregateExpr(e); err == nil {
		t.Fatal("want a duplicate-name error on the second registration")
	}
}

// Registration closes when the server seals, like every other registration.
func TestAggExpr_RegisterAfterSealIsRefused(t *testing.T) {
	type Item struct {
		BaseModel
		Price int `json:"price" db:"price" mfx:"filterable"`
	}
	s := New(Config{})
	_ = s.Register(Item{})
	_ = s.Handler() // seals

	err := s.RegisterAggregateExpr(AggregateExpr{
		Model: "Item", Name: "x", Expr: Col("price"),
	})
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("want ErrRegistrationClosed, got %v", err)
	}
}
