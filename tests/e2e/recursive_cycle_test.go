package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// TestRecursiveQuery_CycleTerminates is the MS-12 regression: before the fix a
// cyclic parent chain made the CTE loop until the process died. The whole test
// binary hit its timeout on a stack dump, so an assertion on the row count
// alone would never have been reached.
func TestRecursiveQuery_CycleTerminates(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})

	aID := srv.MustID(srv.POST("/categories", map[string]any{"name": "A", "status": "active"}))
	bID := srv.MustID(srv.POST("/categories", map[string]any{"name": "B", "parent_id": aID, "status": "active"}))
	srv.PATCH("/categories/"+aID, map[string]any{"parent_id": bID}).AssertStatus(http.StatusOK)

	type result struct {
		rows []any
	}
	done := make(chan result, 1)
	go func() {
		done <- result{srv.GET("/categories/" + aID + "/tree").AssertStatus(http.StatusOK).DataList()}
	}()

	var rows []any
	select {
	case r := <-done:
		rows = r.rows
	case <-time.After(30 * time.Second):
		// Not t.Fatalf inside the goroutine: the query is what hangs, and the
		// test must fail from the side that is still running.
		t.Fatalf("cyclic parent chain did not terminate within 30s")
	}

	// Cycle detection, not merely the depth cap: A and B are each visited once.
	// A depth-cap-only fix would return DefaultRecursiveMaxDepth+1 rows of
	// A,B,A,B,… — terminated, but garbage.
	if len(rows) != 2 {
		t.Fatalf("cyclic tree: want 2 rows (A, B visited once each), got %d", len(rows))
	}
	names := []string{"A", "B"}
	for i, r := range rows {
		if got := r.(map[string]any)["name"]; got != names[i] {
			t.Errorf("row %d: got name %v, want %s", i, got, names[i])
		}
	}
	if d := depths(t, rows); d[0] != 0 || d[1] != 1 {
		t.Errorf("cyclic tree depths: got %v, want [0 1]", d)
	}
}

// TestRecursiveQuery_SelfParentTerminates covers the degenerate one-node cycle:
// a row whose parent is itself. The join matches on the first recursive step,
// so a path guard that only compared against the immediate parent would still
// spin here.
func TestRecursiveQuery_SelfParentTerminates(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})

	aID := srv.MustID(srv.POST("/categories", map[string]any{"name": "A", "status": "active"}))
	srv.PATCH("/categories/"+aID, map[string]any{"parent_id": aID}).AssertStatus(http.StatusOK)

	done := make(chan []any, 1)
	go func() {
		done <- srv.GET("/categories/" + aID + "/tree").AssertStatus(http.StatusOK).DataList()
	}()

	select {
	case rows := <-done:
		if len(rows) != 1 {
			t.Fatalf("self-parent: want 1 row, got %d", len(rows))
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("self-parenting row did not terminate within 30s")
	}
}

// TestRecursiveQuery_AncestorCycleTerminates walks the same cycle upward. The
// ancestors join is the mirror of the descendants one, so a guard applied to
// only one direction would pass the test above and hang here.
func TestRecursiveQuery_AncestorCycleTerminates(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{
			RootID:      id,
			ParentField: "parent_id",
			Direction:   maniflex.RecursiveAncestors,
		}
	})

	aID := srv.MustID(srv.POST("/categories", map[string]any{"name": "A", "status": "active"}))
	bID := srv.MustID(srv.POST("/categories", map[string]any{"name": "B", "parent_id": aID, "status": "active"}))
	srv.PATCH("/categories/"+aID, map[string]any{"parent_id": bID}).AssertStatus(http.StatusOK)

	done := make(chan []any, 1)
	go func() {
		done <- srv.GET("/categories/" + bID + "/tree").AssertStatus(http.StatusOK).DataList()
	}()

	select {
	case rows := <-done:
		if len(rows) != 2 {
			t.Fatalf("ancestor cycle: want 2 rows, got %d", len(rows))
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("ancestor cycle did not terminate within 30s")
	}
}

// TestRecursiveQuery_DefaultDepthCap asserts the zero value now caps rather
// than running unlimited. Builds a chain longer than the cap on acyclic data,
// so only the cap can truncate it — cycle detection is not involved.
func TestRecursiveQuery_DefaultDepthCap(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})

	rootID := srv.MustID(srv.POST("/categories", map[string]any{"name": "n0", "status": "active"}))
	parent := rootID
	const chainLen = maniflex.DefaultRecursiveMaxDepth + 10
	for i := 1; i <= chainLen; i++ {
		parent = srv.MustID(srv.POST("/categories", map[string]any{
			"name": "n", "parent_id": parent, "status": "active",
		}))
	}

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()

	// Depths 0…DefaultRecursiveMaxDepth inclusive.
	if want := maniflex.DefaultRecursiveMaxDepth + 1; len(rows) != want {
		t.Fatalf("default cap: want %d rows, got %d", want, len(rows))
	}
	for _, d := range depths(t, rows) {
		if d > maniflex.DefaultRecursiveMaxDepth {
			t.Errorf("default cap: found row at depth %d", d)
		}
	}
}

// TestRecursiveQuery_NegativeMaxDepthIsUnlimited is the escape hatch: a caller
// who genuinely wants the whole hierarchy can still have it, and the cycle
// guard alone keeps that safe.
func TestRecursiveQuery_NegativeMaxDepthIsUnlimited(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id", MaxDepth: -1}
	})

	rootID := srv.MustID(srv.POST("/categories", map[string]any{"name": "n0", "status": "active"}))
	parent := rootID
	const chainLen = maniflex.DefaultRecursiveMaxDepth + 10
	for i := 1; i <= chainLen; i++ {
		parent = srv.MustID(srv.POST("/categories", map[string]any{
			"name": "n", "parent_id": parent, "status": "active",
		}))
	}

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()
	if want := chainLen + 1; len(rows) != want {
		t.Fatalf("MaxDepth=-1: want the whole chain (%d rows), got %d", want, len(rows))
	}
}

// TestRecursiveQuery_PathColumnNotExposed guards the implementation detail: the
// visited-path column is a means to the cycle guard, not part of the contract.
func TestRecursiveQuery_PathColumnNotExposed(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})
	rootID, _, _, _ := buildCategoryTree(t, srv)

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()
	for i, r := range rows {
		m := r.(map[string]any)
		if _, ok := m["_path"]; ok {
			t.Errorf("row %d leaks the internal _path column: %v", i, m["_path"])
		}
		if _, ok := m["_depth"]; !ok {
			t.Errorf("row %d lost its _depth column", i)
		}
	}
}
