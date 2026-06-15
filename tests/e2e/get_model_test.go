package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestGetModel covers ctx.GetModel() and its five CRUD operations (3D.5).
func TestGetModel(t *testing.T) {
	t.Parallel()

	t.Run("list_returns_records", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.GetModel("User").List(nil)
					if err != nil {
						return fmt.Errorf("GetModel.List: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("Alice", "gm1@x.com", "viewer"))
		srv.POST("/posts", map[string]any{
			"title": "Hello", "body": "World", "status": "draft", "user_id": uid,
		}).AssertStatus(http.StatusCreated)
		srv.GET("/posts").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("want 1 user row, got %d", len(rows))
		}
		if rows[0]["name"] != "Alice" {
			t.Errorf("name: got %v, want Alice", rows[0]["name"])
		}
	})

	t.Run("list_with_filters", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.GetModel("User").List(&maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "role", Operator: maniflex.OpEq, Value: "admin"},
						},
						Page:  1,
						Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("GetModel.List filtered: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("AdminUser", "gm2a@x.com", "admin"))
		srv.CreateUser("Viewer", "gm2b@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.POST("/posts", map[string]any{
			"title": "T", "body": "B", "status": "draft", "user_id": uid,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("want 1 admin row, got %d", len(rows))
		}
		if rows[0]["name"] != "AdminUser" {
			t.Errorf("name: got %v, want AdminUser", rows[0]["name"])
		}
	})

	t.Run("read_returns_single_record", func(t *testing.T) {
		t.Parallel()
		var capturedID string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// After creating a user, read back via GetModel.Read using the
					// ID that came back in the response.
					created, ok := ctx.DBResult.(map[string]any)
					if !ok {
						return nil
					}
					id, _ := created["id"].(string)
					if id == "" {
						return nil
					}
					record, err := ctx.GetModel("User").Read(id)
					if err != nil {
						return fmt.Errorf("GetModel.Read: %w", err)
					}
					mu.Lock()
					capturedID, _ = record["id"].(string)
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("Bob", "gm3@x.com", "viewer"))

		mu.Lock()
		gotID := capturedID
		mu.Unlock()
		if gotID != uid {
			t.Errorf("Read id: got %q, want %q", gotID, uid)
		}
	})

	t.Run("read_not_found_returns_err_not_found", func(t *testing.T) {
		t.Parallel()
		var capturedErr error
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					_, err := ctx.GetModel("User").Read("00000000-0000-0000-0000-000000000000")
					mu.Lock()
					capturedErr = err
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		err := capturedErr
		mu.Unlock()
		if err != maniflex.ErrNotFound {
			t.Errorf("Read missing: got %v, want maniflex.ErrNotFound", err)
		}
	})

	t.Run("create_inserts_record", func(t *testing.T) {
		t.Parallel()
		var capturedID string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/create-via-accessor",
					Handler: func(ctx *maniflex.ServerContext) error {
						row, err := ctx.GetModel("User").Create(map[string]any{
							"name":     "Carol",
							"email":    "gm5@x.com",
							"role":     "viewer",
							"password": "secret",
						})
						if err != nil {
							ctx.Abort(http.StatusInternalServerError, "CREATE_ERR", err.Error())
							return nil
						}
						mu.Lock()
						capturedID, _ = row["id"].(string)
						mu.Unlock()
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusCreated, Data: row}
						return nil
					},
				})
			},
		})

		srv.POST("/create-via-accessor", nil).AssertStatus(http.StatusCreated)

		mu.Lock()
		id := capturedID
		mu.Unlock()
		if id == "" {
			t.Fatal("Create: expected non-empty id")
		}

		// Verify the record is visible through the normal REST endpoint.
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
	})

	t.Run("update_patches_record", func(t *testing.T) {
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/promote/{userId}",
					Handler: func(ctx *maniflex.ServerContext) error {
						id := ctx.URLParam("userId")
						updated, err := ctx.GetModel("User").Update(id, map[string]any{
							"role": "admin",
						})
						if err != nil {
							ctx.Abort(http.StatusInternalServerError, "UPDATE_ERR", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: updated}
						return nil
					},
				})
			},
		})

		uid := srv.MustID(srv.CreateUser("Dave", "gm6@x.com", "viewer"))
		data := srv.POST("/promote/"+uid, nil).AssertStatus(http.StatusOK).Data()
		if data["role"] != "admin" {
			t.Errorf("role after update: got %v, want admin", data["role"])
		}
	})

	t.Run("delete_removes_record", func(t *testing.T) {
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/remove/{userId}",
					Handler: func(ctx *maniflex.ServerContext) error {
						id := ctx.URLParam("userId")
						if err := ctx.GetModel("User").Delete(id); err != nil {
							ctx.Abort(http.StatusInternalServerError, "DELETE_ERR", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusNoContent}
						return nil
					},
				})
			},
		})

		uid := srv.MustID(srv.CreateUser("Eve", "gm7@x.com", "viewer"))
		srv.POST("/remove/"+uid, nil).AssertStatus(http.StatusNoContent)
		srv.GET("/users/" + uid).AssertStatus(http.StatusNotFound)
	})

	t.Run("all_ops_route_through_active_tx", func(t *testing.T) {
		// Create a user via GetModel.Create inside a rolled-back tx — the record
		// must not appear afterwards.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/tx-rollback-test",
					Handler: func(ctx *maniflex.ServerContext) error {
						tx, err := ctx.BeginTx(ctx.Ctx, nil)
						if err != nil {
							return err
						}
						ctx.Tx = tx
						defer tx.Rollback() // always roll back in this test

						_, err = ctx.GetModel("User").Create(map[string]any{
							"name":     "Ghost",
							"email":    "gm8@x.com",
							"role":     "viewer",
							"password": "secret",
						})
						if err != nil {
							return err
						}

						// Confirm the row is visible inside the tx.
						rows, err := ctx.GetModel("User").List(&maniflex.QueryParams{
							Filters: []*maniflex.FilterExpr{
								{Field: "email", Operator: maniflex.OpEq, Value: "gm8@x.com"},
							},
							Page: 1, Limit: 1,
						})
						if err != nil {
							return err
						}
						withinTx := len(rows)

						// Roll back implicitly via defer.
						ctx.Response = &maniflex.APIResponse{
							StatusCode: http.StatusOK,
							Data:       map[string]any{"within_tx": withinTx},
						}
						return nil
					},
				})
			},
		})

		data := srv.POST("/tx-rollback-test", nil).AssertStatus(http.StatusOK).Data()
		if int(data["within_tx"].(float64)) != 1 {
			t.Errorf("within_tx: got %v, want 1", data["within_tx"])
		}

		// After rollback the record must be gone.
		testutil.AssertLen(t, "no ghost after rollback", srv.GET("/users").DataList(), 0)
	})

	t.Run("unknown_model_returns_error_on_first_use", func(t *testing.T) {
		t.Parallel()
		var capturedErr error
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					_, err := ctx.GetModel("NoSuchModel").List(nil)
					mu.Lock()
					capturedErr = err
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		err := capturedErr
		mu.Unlock()
		if err == nil {
			t.Error("expected error for unknown model, got nil")
		}
	})

	t.Run("query_model_backward_compat", func(t *testing.T) {
		// ctx.QueryModel must still work unchanged (it now delegates to GetModel.List).
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.QueryModel("User", nil)
					if err != nil {
						return fmt.Errorf("QueryModel: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Zara", "gm9@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("QueryModel compat: want 1 row, got %d", len(rows))
		}
	})
}
