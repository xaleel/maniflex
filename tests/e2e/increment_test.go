package e2e_test

// WR-2 — atomic column arithmetic, against a real database.
//
// The reason this primitive exists is the concurrency test below: read-then-write
// loses updates under load, silently, because both writers succeed. Everything
// else here is the surface around that — bounds enforced in the same statement,
// the two zero-row outcomes told apart, and the scope carried into the WHERE
// clause rather than checked first.
//
//	go test ./e2e/ -run TestIncrement

import (
	"errors"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type IncItem struct {
	maniflex.BaseModel
	Name     string  `json:"name"     db:"name"`
	Stock    int     `json:"stock"    db:"stock"    mfx:"min:0"`
	Reserved int     `json:"reserved" db:"reserved"`
	Capacity int     `json:"capacity" db:"capacity" mfx:"min:0,max:100"`
	Balance  float64 `json:"balance"  db:"balance"`
}

func incServer(t *testing.T) (*testutil.Server, *maniflex.ServerContext) {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{IncItem{}}})
	return srv, maniflex.NewBackground(t.Context(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
}

func seedItem(t *testing.T, ctx *maniflex.ServerContext, data map[string]any) string {
	t.Helper()
	if _, ok := data["name"]; !ok {
		data["name"] = "widget"
	}
	row, err := ctx.GetModel("IncItem").Create(data)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return row["id"].(string)
}

func stockOf(t *testing.T, ctx *maniflex.ServerContext, id string) int64 {
	t.Helper()
	row, err := ctx.GetModel("IncItem").Read(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	n, ok := toI64(row["stock"])
	if !ok {
		t.Fatalf("stock is %T, not a number", row["stock"])
	}
	return n
}

func toI64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// The whole point. Concurrent increments must all land — read-then-write loses
// some of them and reports success for every one.
func TestIncrement_ConcurrentBumpsAllLand(t *testing.T) {
	srv, ctx := incServer(t)
	_ = srv
	id := seedItem(t, ctx, map[string]any{"stock": 0})

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": 1}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("increment: %v", err)
	}

	if got := stockOf(t, ctx, id); got != workers {
		t.Errorf("stock = %d after %d concurrent increments, want %d — updates were lost",
			got, workers, workers)
	}
}

func TestIncrement_AppliesAndReturnsTheNewValue(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 10})

	row, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": -3})
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if n, _ := toI64(row["stock"]); n != 7 {
		t.Errorf("returned stock = %v, want 7", row["stock"])
	}
	if got := stockOf(t, ctx, id); got != 7 {
		t.Errorf("stored stock = %d, want 7", got)
	}
}

// Several columns in one statement: a transfer between them is never observable
// half-done, and never leaves the pair inconsistent if one bound fails.
func TestIncrement_MovesSeveralColumnsTogether(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 10, "reserved": 0})

	row, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": -3, "reserved": 3})
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}
	s, _ := toI64(row["stock"])
	r, _ := toI64(row["reserved"])
	if s != 7 || r != 3 {
		t.Errorf("stock=%d reserved=%d, want 7/3", s, r)
	}
}

// The bound is enforced by the statement, so "decrement but never below zero"
// needs no read of its own — and a refusal writes nothing at all.
func TestIncrement_BoundRefusesAndWritesNothing(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 2, "reserved": 0})

	_, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": -3, "reserved": 3})
	if !errors.Is(err, maniflex.ErrIncrementOutOfBounds) {
		t.Fatalf("err = %v, want ErrIncrementOutOfBounds", err)
	}
	row, err := ctx.GetModel("IncItem").Read(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s, _ := toI64(row["stock"])
	r, _ := toI64(row["reserved"])
	if s != 2 || r != 0 {
		t.Errorf("a refused increment wrote something: stock=%d reserved=%d, want 2/0", s, r)
	}
}

// Exactly to the bound is allowed; one past it is not. Off-by-one here is the
// difference between "can sell the last unit" and "cannot".
func TestIncrement_BoundIsInclusive(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 3})

	if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": -3}); err != nil {
		t.Fatalf("decrementing exactly to the minimum must be allowed: %v", err)
	}
	if got := stockOf(t, ctx, id); got != 0 {
		t.Fatalf("stock = %d, want 0", got)
	}
	if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": -1}); !errors.Is(err, maniflex.ErrIncrementOutOfBounds) {
		t.Errorf("err = %v, want ErrIncrementOutOfBounds", err)
	}
}

func TestIncrement_MaxBoundApplies(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"capacity": 95})

	if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"capacity": 5}); err != nil {
		t.Fatalf("reaching exactly the maximum must be allowed: %v", err)
	}
	if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"capacity": 1}); !errors.Is(err, maniflex.ErrIncrementOutOfBounds) {
		t.Errorf("err = %v, want ErrIncrementOutOfBounds", err)
	}
}

// The two zero-row outcomes must be told apart: one is permanent, the other may
// succeed on a retry once stock returns.
func TestIncrement_MissingRowIsNotFound(t *testing.T) {
	_, ctx := incServer(t)
	_, err := ctx.GetModel("IncItem").Increment("no-such-id", map[string]any{"stock": 1})
	if !errors.Is(err, maniflex.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, maniflex.ErrIncrementOutOfBounds) {
		t.Error("a missing row must not be reported as a bound violation")
	}
}

// A positive increment must not be judged against a minimum it is moving away
// from — the guard is on the post-increment value for exactly this reason.
func TestIncrement_PositiveDeltaIgnoresTheMinimum(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 0})

	if _, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"stock": 5}); err != nil {
		t.Fatalf("incrementing away from the minimum must be allowed: %v", err)
	}
	if got := stockOf(t, ctx, id); got != 5 {
		t.Errorf("stock = %d, want 5", got)
	}
}

func TestIncrement_FloatColumn(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"balance": 10.5})

	row, err := ctx.GetModel("IncItem").Increment(id, map[string]any{"balance": 2.25})
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if n, ok := row["balance"].(float64); !ok || n != 12.75 {
		t.Errorf("balance = %#v, want 12.75", row["balance"])
	}
}

// Fail closed on anything the statement cannot mean. A silently-dropped column
// would bump updated_at, report success, and leave the counter untouched.
func TestIncrement_RejectsUnusableColumns(t *testing.T) {
	_, ctx := incServer(t)
	id := seedItem(t, ctx, map[string]any{"stock": 1})
	acc := ctx.GetModel("IncItem")

	for name, deltas := range map[string]map[string]any{
		"unknown column":  {"nope": 1},
		"non-numeric":     {"name": 1},
		"non-numeric arg": {"stock": "three"},
		"empty":           {},
	} {
		if _, err := acc.Increment(id, deltas); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}

	// A json name is a near miss worth naming, since Increment takes DB columns.
	_, err := acc.Increment(id, map[string]any{"Stock": 1})
	if err == nil {
		t.Fatal("a wrong-case column was accepted")
	}
	if got := stockOf(t, ctx, id); got != 1 {
		t.Errorf("a rejected increment still wrote: stock = %d, want 1", got)
	}
}
