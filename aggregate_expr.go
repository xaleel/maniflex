package maniflex

import (
	"fmt"
	"reflect"
)

// maxAggExprDepth and maxAggExprNodes bound a registered expression. Both are
// checked at registration, so a pathological expression is a startup error and
// nothing has to be bounded per request.
const (
	maxAggExprDepth = 8
	maxAggExprNodes = 32
)

// Expr is one node of a registered aggregate expression — a column, a numeric
// literal, or an arithmetic combination of the two.
//
// The interface is sealed: isExpr is unexported, so the only way to obtain an
// Expr is through Col, Lit, Add, Sub, Mul and Div. That is the property that
// makes the type safe to put behind a public endpoint — there is no code path
// from a caller-supplied string to SQL. A column name reaches the query only
// after resolving to a real field on the model, and a literal is always a bound
// parameter, never interpolated.
type Expr interface{ isExpr() }

type colExpr struct{ name string }
type litExpr struct{ v float64 }
type binExpr struct {
	op   string
	l, r Expr
}

func (colExpr) isExpr() {}
func (litExpr) isExpr() {}
func (binExpr) isExpr() {}

// Col references a field on the model the expression is registered against. The
// name may be the DB column name or the json name, matching every other field
// reference; it is resolved and checked at registration.
func Col(name string) Expr { return colExpr{name: name} }

// Lit is a numeric constant. It is bound as a query parameter, so it carries no
// injection surface and needs no escaping.
func Lit(v float64) Expr { return litExpr{v: v} }

// Add returns a + b.
func Add(a, b Expr) Expr { return binExpr{op: "+", l: a, r: b} }

// Sub returns a - b.
func Sub(a, b Expr) Expr { return binExpr{op: "-", l: a, r: b} }

// Mul returns a * b.
func Mul(a, b Expr) Expr { return binExpr{op: "*", l: a, r: b} }

// Div returns a / b, with the divisor wrapped in NULLIF(b, 0).
//
// The wrap is what makes the two drivers agree. SQLite answers NULL for a
// division by zero and Postgres raises an error, so an unwrapped division would
// return a row in a SQLite dev run and fail the identical request in Postgres
// production — the same driver-divergence class as the timestamp and boolean
// filter bugs. The cost is that dividing by zero yields NULL rather than
// erroring, on both.
func Div(a, b Expr) Expr { return binExpr{op: "/", l: a, r: b} }

// AggregateExpr declares a named SQL expression on a model, so an aggregate can
// total something the schema does not store as a column — revenue as
// price × count, margin as (price − cost) × count.
//
// It is the counterpart to computing the same figure in Go over paged rows,
// which is what an app does without it: one SQL query instead of a loop that
// has to read every row it sums.
//
//	srv.MustRegisterAggregateExpr(maniflex.AggregateExpr{
//	    Model:   "StoreSiteItem",
//	    Name:    "revenue",
//	    Expr:    maniflex.Mul(maniflex.Col("price"), maniflex.Col("count")),
//	    Exposed: true,
//	})
//
// Once registered, Name is referenceable wherever an aggregate takes a field:
//
//	ctx.Aggregate("StoreSiteItem", maniflex.AggregateQuery{
//	    Select: []maniflex.AggregateField{{Op: maniflex.AggSum, Field: "revenue", As: "total"}},
//	})
//
// Names are resolved and validated at registration, so a typo is a startup
// error naming the field rather than a runtime surprise — the same contract
// Rollup holds.
type AggregateExpr struct {
	// Model is the registered model the expression belongs to.
	Model string

	// Name is what callers reference the expression by. It must not collide
	// with any field on the model, under either spelling, nor with another
	// expression on the same model.
	Name string

	// Expr is the expression tree, built from Col, Lit, Add, Sub, Mul and Div.
	Expr Expr

	// Exposed makes the expression referenceable from the generated
	// GET /:model/aggregate endpoint. It is false by default: server-side
	// callers are trusted, a public endpoint is not, and the decision of what
	// a client may aggregate belongs to the application — the same posture
	// mfx:"filterable" takes for columns.
	//
	// ctx.Aggregate can use the expression either way.
	Exposed bool
}

// compiledAggExpr is a validated AggregateExpr held on the model.
type compiledAggExpr struct {
	name    string
	expr    Expr
	exposed bool
}

// RegisterAggregateExpr installs a named aggregate expression on a model. It
// validates the expression against the registry, so an unknown column, a
// non-numeric one, or a name that collides with a field fails here rather than
// in a query. Must be called before Start()/Handler().
//
// Returns an error rather than panicking so the configuration can be validated
// in a test; MustRegisterAggregateExpr is the panic-on-error variant for main().
func (s *Server) RegisterAggregateExpr(e AggregateExpr) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealedLocked() {
		return fmt.Errorf("%w: RegisterAggregateExpr must be called before Start() or Handler()", ErrRegistrationClosed)
	}
	meta, ok := s.registry.Get(e.Model)
	if !ok {
		return fmt.Errorf("maniflex: aggregate expression %q references unregistered model %q", e.Name, e.Model)
	}
	ce, err := compileAggregateExpr(meta, e)
	if err != nil {
		return err
	}
	return meta.addAggExpr(ce)
}

// MustRegisterAggregateExpr is the panic-on-error variant of
// RegisterAggregateExpr, for use in main() or package initialisation.
func (s *Server) MustRegisterAggregateExpr(e AggregateExpr) {
	if err := s.RegisterAggregateExpr(e); err != nil {
		panic(err)
	}
}

// compileAggregateExpr validates one AggregateExpr against its model.
func compileAggregateExpr(meta *ModelMeta, e AggregateExpr) (compiledAggExpr, error) {
	if e.Name == "" {
		return compiledAggExpr{}, fmt.Errorf("maniflex: aggregate expression on model %q needs a name", meta.Name)
	}
	if e.Expr == nil {
		return compiledAggExpr{}, fmt.Errorf("maniflex: aggregate expression %q has no expression", e.Name)
	}
	// The name becomes a field reference, so it cannot be one already: a
	// collision would make which one a query meant depend on lookup order.
	if f := meta.ResolveFilterField(e.Name); f != nil {
		return compiledAggExpr{}, fmt.Errorf(
			"maniflex: aggregate expression %q collides with a field of that name on model %q",
			e.Name, meta.Name)
	}
	depth, nodes, err := validateExprTree(meta, e.Expr, 1)
	if err != nil {
		return compiledAggExpr{}, err
	}
	if depth > maxAggExprDepth {
		return compiledAggExpr{}, fmt.Errorf(
			"maniflex: aggregate expression %q is nested %d deep, over the limit of %d",
			e.Name, depth, maxAggExprDepth)
	}
	if nodes > maxAggExprNodes {
		return compiledAggExpr{}, fmt.Errorf(
			"maniflex: aggregate expression %q has %d nodes, over the limit of %d",
			e.Name, nodes, maxAggExprNodes)
	}
	return compiledAggExpr{name: e.Name, expr: e.Expr, exposed: e.Exposed}, nil
}

// validateExprTree walks the tree, checking every column reference and
// returning the tree's depth and node count.
func validateExprTree(meta *ModelMeta, e Expr, depth int) (int, int, error) {
	switch n := e.(type) {
	case colExpr:
		f := meta.ResolveFilterField(n.name)
		if f == nil {
			return 0, 0, fmt.Errorf(
				"maniflex: aggregate expression references %q, which is not a field on model %q",
				n.name, meta.Name)
		}
		if !isNumericKind(f.Type) {
			return 0, 0, fmt.Errorf(
				"maniflex: aggregate expression references %q on model %q, which is %s and not numeric",
				n.name, meta.Name, f.Type)
		}
		return depth, 1, nil
	case litExpr:
		return depth, 1, nil
	case binExpr:
		if n.l == nil || n.r == nil {
			return 0, 0, fmt.Errorf("maniflex: aggregate expression has an incomplete %q operation", n.op)
		}
		// Bail out early on a tree deep enough to be pathological, so a
		// degenerate input cannot make validation itself the expensive part.
		if depth > maxAggExprDepth {
			return depth, 1, nil
		}
		ld, ln, err := validateExprTree(meta, n.l, depth+1)
		if err != nil {
			return 0, 0, err
		}
		rd, rn, err := validateExprTree(meta, n.r, depth+1)
		if err != nil {
			return 0, 0, err
		}
		return max(ld, rd), ln + rn + 1, nil
	}
	return 0, 0, fmt.Errorf("maniflex: unsupported aggregate expression node %T", e)
}

// isNumericKind reports whether t is an integer or float type, dereferencing a
// pointer. Arithmetic on anything else is a mistake worth catching at startup.
func isNumericKind(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// renderAggExpr renders a validated expression tree as SQL, binding every
// literal as a parameter.
func renderAggExpr(meta *ModelMeta, e Expr, pb *aggPH) (string, error) {
	switch n := e.(type) {
	case colExpr:
		f := meta.ResolveFilterField(n.name)
		if f == nil {
			// Unreachable for a registered expression — compileAggregateExpr
			// checked every column. Kept so a hand-built tree cannot reach the
			// database with an unresolved name.
			return "", fmt.Errorf("maniflex: aggregate expression references unknown field %q", n.name)
		}
		return Quote(meta.TableName) + "." + Quote(f.Tags.DBName), nil
	case litExpr:
		return pb.add(n.v), nil
	case binExpr:
		l, err := renderAggExpr(meta, n.l, pb)
		if err != nil {
			return "", err
		}
		r, err := renderAggExpr(meta, n.r, pb)
		if err != nil {
			return "", err
		}
		if n.op == "/" {
			// See Div: NULLIF makes both drivers answer NULL rather than one
			// answering NULL and the other raising.
			return fmt.Sprintf("(%s / NULLIF(%s, 0))", l, r), nil
		}
		return fmt.Sprintf("(%s %s %s)", l, n.op, r), nil
	}
	return "", fmt.Errorf("maniflex: unsupported aggregate expression node %T", e)
}

// addAggExpr installs a compiled expression on the model, refusing a duplicate
// name.
func (m *ModelMeta) addAggExpr(ce compiledAggExpr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.aggExprs == nil {
		m.aggExprs = make(map[string]compiledAggExpr)
	}
	if _, exists := m.aggExprs[ce.name]; exists {
		return fmt.Errorf("maniflex: aggregate expression %q is already registered on model %q", ce.name, m.Name)
	}
	m.aggExprs[ce.name] = ce
	return nil
}

// AggExpr returns the registered aggregate expression of that name, and whether
// one exists. Exported so an adapter or a custom endpoint can resolve the same
// names ctx.Aggregate does.
func (m *ModelMeta) AggExpr(name string) (found, exposed bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ce, ok := m.aggExprs[name]
	return ok, ok && ce.exposed
}

// aggExprTree returns the expression tree registered under name.
func (m *ModelMeta) aggExprTree(name string) (Expr, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ce, ok := m.aggExprs[name]
	if !ok {
		return nil, false
	}
	return ce.expr, true
}
