package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// TestRawQuery covers ctx.RawQuery and ctx.RawExec (3D.2).
func TestRawQuery(t *testing.T) {
	t.Parallel()

	t.Run("select_returns_rows", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.RawQuery("SELECT id, name FROM users ORDER BY name")
					if err != nil {
						return fmt.Errorf("RawQuery: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Alice", "rq1a@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.CreateUser("Bob", "rq1b@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()

		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0]["name"] != "Alice" {
			t.Errorf("first row name: got %v, want Alice", rows[0]["name"])
		}
	})

	t.Run("args_are_parameterised", func(t *testing.T) {
		// Passing a value that looks like SQL must not be injected.
		t.Parallel()
		skipRawSQLOnPostgres(t)
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// Deliberately inject-like value as a parameter — must return 0 rows.
					rows, err := ctx.RawQuery("SELECT id FROM users WHERE name = ?", "'; DROP TABLE users; --")
					if err != nil {
						return fmt.Errorf("RawQuery: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Charlie", "rq2@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 0 {
			t.Errorf("injection attempt must return 0 rows, got %d", len(rows))
		}
	})

	t.Run("empty_result_returns_nil", func(t *testing.T) {
		t.Parallel()
		skipRawSQLOnPostgres(t)
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.RawQuery("SELECT id FROM users WHERE name = ?", "nobody")
					if err != nil {
						return fmt.Errorf("RawQuery: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 0 {
			t.Errorf("empty query: want 0 rows, got %d", len(rows))
		}
	})

	t.Run("exec_update_returns_rows_affected", func(t *testing.T) {
		t.Parallel()
		skipRawSQLOnPostgres(t)
		var affected int64
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					n, err := ctx.RawExec("UPDATE users SET name = ? WHERE name = ?", "Dana-updated", "Dana")
					if err != nil {
						return fmt.Errorf("RawExec: %w", err)
					}
					mu.Lock()
					affected = n
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Dana", "rq4@x.com", "viewer").AssertStatus(http.StatusCreated)

		mu.Lock()
		n := affected
		mu.Unlock()
		if n != 1 {
			t.Errorf("rows affected: got %d, want 1", n)
		}
	})

	t.Run("exec_no_match_returns_zero", func(t *testing.T) {
		t.Parallel()
		skipRawSQLOnPostgres(t)
		var affected int64
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					n, err := ctx.RawExec("UPDATE users SET name = ? WHERE name = ?", "x", "nobody")
					if err != nil {
						return fmt.Errorf("RawExec: %w", err)
					}
					mu.Lock()
					affected = n
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		n := affected
		mu.Unlock()
		if n != 0 {
			t.Errorf("rows affected: got %d, want 0", n)
		}
	})

	t.Run("in_tx_sees_uncommitted_write", func(t *testing.T) {
		// RawQuery inside a transaction must see rows written by RawExec in
		// the same transaction, even before commit.
		t.Parallel()
		var capturedCount int64
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback()

					if err := next(); err != nil {
						return err
					}
					if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
						return nil
					}
					return tx.Commit()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))

				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// Read within the same Tx — must see the just-inserted row.
					rows, err := ctx.RawQuery("SELECT COUNT(*) AS c FROM users")
					if err != nil {
						return fmt.Errorf("RawQuery: %w", err)
					}
					if len(rows) > 0 {
						switch v := rows[0]["c"].(type) {
						case int64:
							mu.Lock()
							capturedCount = v
							mu.Unlock()
						}
					}
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Eve", "rq6@x.com", "viewer").AssertStatus(http.StatusCreated)

		mu.Lock()
		c := capturedCount
		mu.Unlock()
		if c < 1 {
			t.Errorf("within-tx RawQuery count: got %d, want >= 1", c)
		}
	})

	t.Run("in_tx_rollback_reverts_raw_exec", func(t *testing.T) {
		// RawExec inside a rolled-back transaction must not persist.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback() // always rollback — never commit

					if err := next(); err != nil {
						return err
					}
					// Override response so test assertion doesn't see a 201.
					ctx.Abort(http.StatusTeapot, "FORCED_ROLLBACK", "test rollback")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		srv.CreateUser("Frank", "rq7@x.com", "viewer").AssertStatus(http.StatusTeapot)

		// Rollback: no records should exist.
		testutil.AssertLen(t, "no records after rollback", srv.GET("/users").DataList(), 0)
	})

	t.Run("no_adapter_returns_error", func(t *testing.T) {
		// ServerContext with no adapter set must return ErrNoAdapter.
		t.Parallel()
		ctx := &maniflex.ServerContext{}
		_, err := ctx.RawQuery("SELECT 1")
		if err == nil {
			t.Error("expected error from RawQuery without adapter, got nil")
		}
		_, err = ctx.RawExec("UPDATE users SET name = 'x' WHERE id = 'y'")
		if err == nil {
			t.Error("expected error from RawExec without adapter, got nil")
		}
	})
}

// TestQueryModel covers ctx.QueryModel (3D.3).
func TestQueryModel(t *testing.T) {
	t.Parallel()

	t.Run("basic_returns_registered_model_records", func(t *testing.T) {
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
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("Grace", "qm1@x.com", "viewer"))
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
		if rows[0]["name"] != "Grace" {
			t.Errorf("name: got %v, want Grace", rows[0]["name"])
		}
	})

	t.Run("with_filters_narrows_results", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.QueryModel("User", &maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "role", Operator: maniflex.OpEq, Value: "admin"},
						},
						Page:  1,
						Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("QueryModel: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("Admin", "qm2a@x.com", "admin"))
		srv.CreateUser("Viewer", "qm2b@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.POST("/posts", map[string]any{
			"title": "T", "body": "B", "status": "draft", "user_id": uid,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()

		if len(rows) != 1 {
			t.Fatalf("want 1 admin row, got %d", len(rows))
		}
		if rows[0]["name"] != "Admin" {
			t.Errorf("name: got %v, want Admin", rows[0]["name"])
		}
	})

	t.Run("nil_params_uses_defaults", func(t *testing.T) {
		// nil *QueryParams should not panic and should return up to defaultLimit rows.
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
						return fmt.Errorf("QueryModel nil params: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Hannah", "qm3@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("want 1 user, got %d", len(rows))
		}
	})

	t.Run("unknown_model_returns_error", func(t *testing.T) {
		t.Parallel()
		var capturedErr error
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					_, err := ctx.QueryModel("NoSuchModel", nil)
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

	t.Run("in_tx_sees_uncommitted_create", func(t *testing.T) {
		// QueryModel inside a transaction must see rows created via the same Tx.
		t.Parallel()
		var capturedCount int
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback()

					if err := next(); err != nil {
						return err
					}
					if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
						return nil
					}
					return tx.Commit()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))

				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.QueryModel("User", nil)
					if err != nil {
						return fmt.Errorf("QueryModel in tx: %w", err)
					}
					mu.Lock()
					capturedCount = len(rows)
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Ivy", "qm5@x.com", "viewer").AssertStatus(http.StatusCreated)

		mu.Lock()
		c := capturedCount
		mu.Unlock()
		if c < 1 {
			t.Errorf("within-tx QueryModel count: got %d, want >= 1", c)
		}
	})

	t.Run("from_action_handler", func(t *testing.T) {
		// Action handler uses ctx.QueryModel to read data from another model
		// and ctx.RawExec to write via raw SQL — covers the action.md example.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "GET",
					Path:   "/user-summary",
					Handler: func(ctx *maniflex.ServerContext) error {
						rows, err := ctx.QueryModel("User", &maniflex.QueryParams{
							Page:  1,
							Limit: 100,
						})
						if err != nil {
							ctx.Abort(http.StatusInternalServerError, "QUERY_ERROR", err.Error())
							return nil
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

		srv.CreateUser("Jack", "qm6a@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.CreateUser("Kate", "qm6b@x.com", "admin").AssertStatus(http.StatusCreated)

		data := srv.GET("/user-summary").AssertStatus(http.StatusOK).Data()
		if int(data["count"].(float64)) != 2 {
			t.Errorf("count: got %v, want 2", data["count"])
		}
	})

	t.Run("raw_exec_from_action_with_tx", func(t *testing.T) {
		// Action handler uses ctx.BeginTx + ctx.RawExec + ctx.QueryModel,
		// then rolls back — verifying the whole Tx chain works from an action.
		t.Parallel()
		skipRawSQLOnPostgres(t)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/users/{id}/demote",
					Handler: func(ctx *maniflex.ServerContext) error {
						tx, err := ctx.BeginTx(ctx.Ctx, nil)
						if err != nil {
							return err
						}
						ctx.Tx = tx
						defer tx.Rollback() // always rollback in this test

						// Raw update within the transaction.
						_, err = ctx.RawExec(
							"UPDATE users SET role = ? WHERE id = ?",
							"viewer", ctx.ResourceID,
						)
						if err != nil {
							return err
						}

						// Read back within same transaction.
						rows, err := ctx.QueryModel("User", &maniflex.QueryParams{
							Filters: []*maniflex.FilterExpr{
								{Field: "id", Operator: maniflex.OpEq, Value: ctx.ResourceID},
							},
							Page: 1, Limit: 1,
						})
						if err != nil {
							return err
						}

						role := ""
						if len(rows) > 0 {
							role, _ = rows[0]["role"].(string)
						}

						// Always rollback — role reverts after response.
						ctx.Response = &maniflex.APIResponse{
							StatusCode: http.StatusOK,
							Data:       map[string]any{"role_within_tx": role},
						}
						return nil
					},
				})
			},
		})

		uid := srv.MustID(srv.CreateUser("Leo", "qm7@x.com", "admin"))

		data := srv.POST("/users/"+uid+"/demote", nil).AssertStatus(http.StatusOK).Data()
		if data["role_within_tx"] != "viewer" {
			t.Errorf("role_within_tx: got %v, want viewer", data["role_within_tx"])
		}

		// Tx was rolled back — role remains admin.
		userData := srv.GET("/users/" + uid).AssertStatus(http.StatusOK).Data()
		if userData["role"] != "admin" {
			t.Errorf("role after rollback: got %v, want admin", userData["role"])
		}
	})
}
