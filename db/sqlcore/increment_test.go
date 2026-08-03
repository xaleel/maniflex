package sqlcore

// WR-2 — the shape of the increment statement, on both dialects.
//
// The behaviour is exercised end-to-end in tests/e2e against a real database on
// whichever lane is running. These are the assertions that cannot be made there:
// the local lane is SQLite, so the Postgres placeholder rendering is checked as
// the string it is, and the guard's algebra is checked where it is legible.
//
//	go test ./db/sqlcore/ -run TestIncrementStmt

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

func incModel(t *testing.T) *maniflex.ModelMeta {
	t.Helper()
	type IncItem struct {
		maniflex.BaseModel
		Stock    int `json:"stock"    db:"stock"    mfx:"min:0"`
		Reserved int `json:"reserved" db:"reserved"`
		Capacity int `json:"capacity" db:"capacity" mfx:"min:0,max:100"`
	}
	meta, err := maniflex.ScanModel(IncItem{}, maniflex.ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	return meta
}

// The guard says what it means: the bound applies to the value AFTER the
// increment. Writing it as `"stock" >= 3` instead would be wrong for a positive
// delta, refusing an increment that could not possibly breach a minimum.
func TestIncrementStmt_GuardsThePostIncrementValue(t *testing.T) {
	meta := incModel(t)
	bounds := maniflex.IncrementBounds(meta, map[string]any{"stock": -3})

	query, args := incrementStmt(meta, "id-1", map[string]any{"stock": -3},
		bounds, nil, maniflex.SQLite)

	if !strings.Contains(query, `SET "stock" = "stock" + ?`) {
		t.Errorf("the SET is not relative to the stored value:\n%s", query)
	}
	if !strings.Contains(query, `"stock" + ? >= ?`) {
		t.Errorf("the guard is not on the post-increment value:\n%s", query)
	}
	// The delta is bound twice — once for the SET, once for the guard — because
	// on SQLite a repeated ? consumes the NEXT argument rather than the same one.
	deltas := 0
	for _, a := range args {
		if n, ok := a.(int); ok && n == -3 {
			deltas++
		}
	}
	if deltas != 2 {
		t.Errorf("the delta is bound %d time(s), want 2 (SET + guard); args=%v", deltas, args)
	}
}

// The arguments must appear in the same order as the "?" they belong to.
//
// SQLite binds a "?" by its position in the SQL text, so a placeholder
// registered out of order silently takes a different argument — which is not a
// syntax error and not a wrong answer either, but a wrong WRITE. Building the
// bound guards while emitting the SET clauses, their natural place, put the
// bound's value where updated_at's belonged and scrambled everything after it,
// so every bounded increment was refused. Postgres would have been unaffected,
// $N binding by number, so only the SQLite lane can catch this.
func TestIncrementStmt_ArgumentsFollowTextOrder(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"stock": -3}
	query, args := incrementStmt(meta, "id-1", deltas,
		maniflex.IncrementBounds(meta, deltas), nil, maniflex.SQLite)

	// SET stock = stock + ?, updated_at = ? WHERE id = ? AND stock + ? >= ?
	if n := strings.Count(query, "?"); n != len(args) {
		t.Fatalf("%d placeholders but %d args:\n%s", n, len(args), query)
	}
	if len(args) != 5 {
		t.Fatalf("expected 5 args (delta, updated_at, id, delta, min), got %d: %v", len(args), args)
	}
	if v, ok := args[0].(int); !ok || v != -3 {
		t.Errorf("args[0] = %#v, want the delta -3 (the SET)", args[0])
	}
	if _, ok := args[1].(string); !ok {
		t.Errorf("args[1] = %#v, want the updated_at timestamp", args[1])
	}
	if args[2] != "id-1" {
		t.Errorf("args[2] = %#v, want the id", args[2])
	}
	if v, ok := args[3].(int); !ok || v != -3 {
		t.Errorf("args[3] = %#v, want the delta -3 again (the guard)", args[3])
	}
	if v, ok := args[4].(float64); !ok || v != 0 {
		t.Errorf("args[4] = %#v, want the min bound 0", args[4])
	}
}

// A column with no declared bounds must produce no guard at all, or every
// counter pays for a constraint it never asked for.
func TestIncrementStmt_UnboundedColumnHasNoGuard(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"reserved": 5}
	query, _ := incrementStmt(meta, "id-1", deltas,
		maniflex.IncrementBounds(meta, deltas), nil, maniflex.SQLite)

	if strings.Contains(query, ">=") || strings.Contains(query, "<=") {
		t.Errorf("an unbounded column produced a guard:\n%s", query)
	}
}

// Both sides of a two-sided bound must reach the statement.
func TestIncrementStmt_MinAndMaxBothApply(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"capacity": 10}
	query, _ := incrementStmt(meta, "id-1", deltas,
		maniflex.IncrementBounds(meta, deltas), nil, maniflex.SQLite)

	if !strings.Contains(query, `"capacity" + ? >= ?`) {
		t.Errorf("the min guard is missing:\n%s", query)
	}
	if !strings.Contains(query, `"capacity" + ? <= ?`) {
		t.Errorf("the max guard is missing:\n%s", query)
	}
}

// Several columns move in one statement, which is what makes a transfer between
// them impossible to observe half-done.
func TestIncrementStmt_MultipleColumnsInOneStatement(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"stock": -3, "reserved": 3}
	query, _ := incrementStmt(meta, "id-1", deltas,
		maniflex.IncrementBounds(meta, deltas), nil, maniflex.SQLite)

	if strings.Count(query, "UPDATE") != 1 {
		t.Errorf("expected exactly one UPDATE:\n%s", query)
	}
	if !strings.Contains(query, `"reserved" = "reserved" + ?`) ||
		!strings.Contains(query, `"stock" = "stock" + ?`) {
		t.Errorf("both columns must be in the SET:\n%s", query)
	}
	// Deterministic column order, so the same call always renders the same SQL.
	if strings.Index(query, `"reserved"`) > strings.Index(query, `"stock" =`) {
		t.Errorf("columns are not in sorted order:\n%s", query)
	}
}

// Postgres rejects "?" outright, so a stray one is a syntax error on every call
// rather than a subtly wrong query.
func TestIncrementStmt_PostgresPlaceholders(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"stock": -3}
	query, args := incrementStmt(meta, "id-1", deltas,
		maniflex.IncrementBounds(meta, deltas), nil, maniflex.Postgres)

	if strings.Contains(query, "?") {
		t.Errorf("a ? placeholder survived on the Postgres path:\n%s", query)
	}
	if !strings.Contains(query, "$1") {
		t.Errorf("no $1 placeholder:\n%s", query)
	}
	// Every argument must have a placeholder and vice versa; an off-by-one here
	// is a runtime "bind message supplies N parameters" failure.
	for i := range args {
		if !strings.Contains(query, "$"+itoa(i+1)) {
			t.Errorf("argument %d has no placeholder:\n%s", i+1, query)
		}
	}
}

// updated_at moves with the counter, as it does for an ordinary Update — a row
// whose value changed but whose timestamp did not is invisible to every
// incremental sync built on that column.
func TestIncrementStmt_BumpsUpdatedAt(t *testing.T) {
	meta := incModel(t)
	deltas := map[string]any{"reserved": 1}
	query, _ := incrementStmt(meta, "id-1", deltas, nil, nil, maniflex.SQLite)
	if !strings.Contains(query, `"updated_at" = ?`) {
		t.Errorf("updated_at is not bumped:\n%s", query)
	}
}

// The forced scope has to be in the statement, not checked beforehand: a
// pre-flight read leaves a window in which the scope can change, and costs a
// round trip on the path whose whole point is to be one.
func TestIncrementStmt_ForcedScopeIsInTheStatement(t *testing.T) {
	meta := incModel(t)
	qp := &maniflex.QueryParams{Filters: []*maniflex.FilterExpr{
		{Field: "id", Operator: maniflex.OpEq, Value: "id-1", Forced: true},
	}}
	deltas := map[string]any{"reserved": 1}
	query, _ := incrementStmt(meta, "id-1", deltas, nil, qp, maniflex.SQLite)

	if !strings.Contains(query, "IN (SELECT") {
		t.Errorf("the forced scope did not reach the WHERE clause:\n%s", query)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
