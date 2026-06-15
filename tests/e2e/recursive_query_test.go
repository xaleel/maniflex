package e2e

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// category is a self-referential model used exclusively by the recursive-query tests.
type category struct {
	maniflex.BaseModel
	Name     string `json:"name"      mfx:"required,filterable"`
	ParentID string `json:"parent_id"`
	Status   string `json:"status"    mfx:"filterable,default:active"`
}

// buildCategoryTree seeds a 4-node tree and returns the IDs:
//
//	root       (depth 0)
//	├── childA  (depth 1)
//	│   └── grandchild (depth 2)
//	└── childB  (depth 1)
func buildCategoryTree(t *testing.T, srv *testutil.Server) (rootID, childAID, grandchildID, childBID string) {
	t.Helper()
	rootID = srv.MustID(srv.POST("/categories", map[string]any{"name": "Root", "status": "active"}))
	childAID = srv.MustID(srv.POST("/categories", map[string]any{"name": "Child A", "parent_id": rootID, "status": "active"}))
	grandchildID = srv.MustID(srv.POST("/categories", map[string]any{"name": "Grandchild", "parent_id": childAID, "status": "active"}))
	childBID = srv.MustID(srv.POST("/categories", map[string]any{"name": "Child B", "parent_id": rootID, "status": "active"}))
	return
}

// categoryServer builds a test server with the category model and registers a
// /categories/{id}/tree action that calls ctx.RecursiveQuery with the
// given options builder.
func categoryServer(t *testing.T, optsBuilder func(id string) maniflex.RecursiveQuery) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{category{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/categories/{id}/tree",
				Handler: func(ctx *maniflex.ServerContext) error {
					rows, err := ctx.RecursiveQuery("category", optsBuilder(ctx.ResourceID))
					if err != nil {
						ctx.Abort(http.StatusInternalServerError, "QUERY_ERROR", err.Error())
						return nil
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: rows}
					return nil
				},
			})
		},
	})
}

// depths extracts the _depth values from a DataList response as []int.
func depths(t *testing.T, rows []any) []int {
	t.Helper()
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = int(r.(map[string]any)["_depth"].(float64))
	}
	return out
}

func TestRecursiveQuery_Descendants(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})
	rootID, _, _, _ := buildCategoryTree(t, srv)

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()

	if len(rows) != 4 {
		t.Fatalf("want 4 rows (root + 3 descendants), got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "Root" {
		t.Errorf("row 0 name: got %v, want Root", first["name"])
	}
	wantDepths := []int{0, 1, 1, 2}
	for i, d := range depths(t, rows) {
		if d != wantDepths[i] {
			t.Errorf("row %d depth: got %d, want %d", i, d, wantDepths[i])
		}
	}
}

func TestRecursiveQuery_Ancestors(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{category{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/categories/{id}/tree",
				Handler: func(ctx *maniflex.ServerContext) error {
					rows, err := ctx.RecursiveQuery("category", maniflex.RecursiveQuery{
						RootID:      ctx.ResourceID,
						ParentField: "parent_id",
						Direction:   maniflex.RecursiveAncestors,
					})
					if err != nil {
						ctx.Abort(http.StatusInternalServerError, "QUERY_ERROR", err.Error())
						return nil
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: rows}
					return nil
				},
			})
		},
	})
	_, _, grandchildID, _ := buildCategoryTree(t, srv)

	rows := srv.GET("/categories/" + grandchildID + "/tree").AssertStatus(http.StatusOK).DataList()

	if len(rows) != 3 {
		t.Fatalf("want 3 rows (grandchild→childA→root), got %d", len(rows))
	}
	if rows[0].(map[string]any)["name"] != "Grandchild" {
		t.Errorf("row 0: want Grandchild, got %v", rows[0].(map[string]any)["name"])
	}
	if rows[2].(map[string]any)["name"] != "Root" {
		t.Errorf("row 2: want Root, got %v", rows[2].(map[string]any)["name"])
	}
	for i, d := range depths(t, rows) {
		if d != i {
			t.Errorf("row %d depth: got %d, want %d", i, d, i)
		}
	}
}

func TestRecursiveQuery_MaxDepth(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id", MaxDepth: 1}
	})
	rootID, _, _, _ := buildCategoryTree(t, srv)

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()

	// MaxDepth=1 → root (depth 0) + 2 immediate children (depth 1) = 3 rows.
	// Grandchild at depth 2 must not appear.
	if len(rows) != 3 {
		t.Fatalf("MaxDepth=1: want 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if d := int(r.(map[string]any)["_depth"].(float64)); d > 1 {
			t.Errorf("MaxDepth=1: found row at depth %d", d)
		}
	}
}

func TestRecursiveQuery_WhereFilter(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{
			RootID:      id,
			ParentField: "parent_id",
			Where: []*maniflex.FilterExpr{
				{Field: "status", Operator: maniflex.OpEq, Value: "active"},
			},
		}
	})

	// Root (active) → Child A (active) → Grandchild (inactive)
	//              → Child B (inactive)
	rootID := srv.MustID(srv.POST("/categories", map[string]any{"name": "Root", "status": "active"}))
	childAID := srv.MustID(srv.POST("/categories", map[string]any{"name": "Child A", "parent_id": rootID, "status": "active"}))
	srv.POST("/categories", map[string]any{"name": "Grandchild", "parent_id": childAID, "status": "inactive"}).AssertStatus(http.StatusCreated)
	srv.POST("/categories", map[string]any{"name": "Child B", "parent_id": rootID, "status": "inactive"}).AssertStatus(http.StatusCreated)

	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()

	// Only Root and Child A pass the status=active filter.
	if len(rows) != 2 {
		t.Fatalf("where filter: want 2 rows (Root + Child A), got %d", len(rows))
	}
	names := make(map[string]bool, 2)
	for _, r := range rows {
		names[r.(map[string]any)["name"].(string)] = true
	}
	if !names["Root"] || !names["Child A"] {
		t.Errorf("where filter: got names %v, want Root and Child A", names)
	}
}

func TestRecursiveQuery_NonexistentRoot(t *testing.T) {
	t.Parallel()

	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})

	rows := srv.GET("/categories/00000000-0000-0000-0000-000000000000/tree").AssertStatus(http.StatusOK).DataList()
	if len(rows) != 0 {
		t.Errorf("nonexistent root: want 0 rows, got %d", len(rows))
	}
}

func TestRecursiveQuery_WithinTransaction(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{category{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/categories/{id}/tree-in-tx",
				Handler: func(ctx *maniflex.ServerContext) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback()

					rows, err := ctx.RecursiveQuery("category", maniflex.RecursiveQuery{
						RootID:      ctx.ResourceID,
						ParentField: "parent_id",
					})
					if err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"count": len(rows)},
					}
					return nil
				},
			})
		},
	})

	rootID := srv.MustID(srv.POST("/categories", map[string]any{"name": "Root", "status": "active"}))
	srv.POST("/categories", map[string]any{"name": "Child", "parent_id": rootID, "status": "active"}).AssertStatus(http.StatusCreated)

	data := srv.GET("/categories/" + rootID + "/tree-in-tx").AssertStatus(http.StatusOK).Data()
	if int(data["count"].(float64)) != 2 {
		t.Errorf("in-tx RecursiveQuery: want count 2, got %v", data["count"])
	}
}

func TestRecursiveQuery_ValidationErrors(t *testing.T) {
	t.Parallel()

	t.Run("no_registry", func(t *testing.T) {
		t.Parallel()
		ctx := &maniflex.ServerContext{}
		_, err := ctx.RecursiveQuery("category", maniflex.RecursiveQuery{RootID: "x", ParentField: "parent_id"})
		if err == nil {
			t.Error("expected error without registry")
		}
	})

	t.Run("empty_root_id", func(t *testing.T) {
		t.Parallel()
		ctx := &maniflex.ServerContext{}
		_, err := ctx.RecursiveQuery("category", maniflex.RecursiveQuery{ParentField: "parent_id"})
		if err == nil {
			t.Error("expected error for empty RootID")
		}
	})

	t.Run("empty_parent_field", func(t *testing.T) {
		t.Parallel()
		ctx := &maniflex.ServerContext{}
		_, err := ctx.RecursiveQuery("category", maniflex.RecursiveQuery{RootID: "x"})
		if err == nil {
			t.Error("expected error for empty ParentField")
		}
	})

	t.Run("unknown_parent_field", func(t *testing.T) {
		t.Parallel()
		var capturedErr error
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{category{}},
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "GET",
					Path:   "/categories/check",
					Handler: func(ctx *maniflex.ServerContext) error {
						_, capturedErr = ctx.RecursiveQuery("category", maniflex.RecursiveQuery{
							RootID:      "x",
							ParentField: "nonexistent_col",
						})
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
						return nil
					},
				})
			},
		})
		srv.GET("/categories/check").AssertStatus(http.StatusOK)
		if capturedErr == nil {
			t.Error("expected error for unknown ParentField")
		}
	})

	t.Run("unknown_where_field", func(t *testing.T) {
		t.Parallel()
		var capturedErr error
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{category{}},
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "GET",
					Path:   "/categories/check",
					Handler: func(ctx *maniflex.ServerContext) error {
						_, capturedErr = ctx.RecursiveQuery("category", maniflex.RecursiveQuery{
							RootID:      "x",
							ParentField: "parent_id",
							Where: []*maniflex.FilterExpr{
								{Field: "no_such_col", Operator: maniflex.OpEq, Value: "v"},
							},
						})
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
						return nil
					},
				})
			},
		})
		srv.GET("/categories/check").AssertStatus(http.StatusOK)
		if capturedErr == nil {
			t.Error("expected error for unknown Where field")
		}
	})
}
