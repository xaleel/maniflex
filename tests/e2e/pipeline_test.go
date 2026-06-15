package e2e

import (
	"net/http"
	"sync"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// TestPipeline tests middleware registration, ordering, scoping, and
// short-circuit behaviour through the full HTTP stack.
func TestPipeline(t *testing.T) {
	t.Parallel()

	// ── Execution order ───────────────────────────────────────────────────────

	t.Run("steps_execute_in_order_auth_deserialize_validate_service_db_response", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var order []string

		step := func(name string) maniflex.MiddlewareFunc {
			return func(ctx *maniflex.ServerContext, next func() error) error {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return next()
			}
		}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(step("auth"))
				s.Pipeline.Deserialize.Register(step("deserialize"))
				s.Pipeline.Validate.Register(step("validate"))
				s.Pipeline.Service.Register(step("service"))
				s.Pipeline.DB.Register(step("db"), maniflex.AtPosition(maniflex.After))
				s.Pipeline.Response.Register(step("response"), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "A", "email": "order@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		got := make([]string, len(order))
		copy(got, order)
		mu.Unlock()

		want := []string{"auth", "deserialize", "validate", "service", "db", "response"}
		if len(got) < len(want) {
			t.Fatalf("expected %d steps, got %d: %v", len(want), len(got), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("step[%d]: got %q, want %q (full order: %v)", i, got[i], w, got)
			}
		}
	})

	t.Run("before_fires_before_default_after_fires_after", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var order []string

		record := func(name string) maniflex.MiddlewareFunc {
			return func(ctx *maniflex.ServerContext, next func() error) error {
				mu.Lock()
				order = append(order, "before:"+name)
				mu.Unlock()
				err := next()
				mu.Lock()
				order = append(order, "after:"+name)
				mu.Unlock()
				return err
			}
		}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// Register After first to prove it doesn't affect order
				s.Pipeline.Service.Register(record("svc"), maniflex.AtPosition(maniflex.After))
				s.Pipeline.Service.Register(record("svc"), maniflex.AtPosition(maniflex.Before))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "B", "email": "order2@x.com", "password": "s",
		})

		mu.Lock()
		got := make([]string, len(order))
		copy(got, order)
		mu.Unlock()

		// before:svc → (default service noop) → after:svc
		testutil.AssertEqual(t, "first", got[0], "before:svc")
		testutil.AssertEqual(t, "last", got[len(got)-1], "after:svc")
	})

	// ── Short-circuit behaviour ───────────────────────────────────────────────

	t.Run("abort_in_auth_prevents_db_from_running", func(t *testing.T) {
		t.Parallel()
		dbCalled := false

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "no auth")
					return nil // do NOT call next
				})
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					dbCalled = true
					return next()
				}, maniflex.AtPosition(maniflex.Replace))
			},
		})
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusUnauthorized)
		if dbCalled {
			t.Error("DB step must not run when Auth aborts")
		}
	})

	t.Run("abort_in_validate_prevents_service_and_db", func(t *testing.T) {
		t.Parallel()
		var svcCalled, dbCalled bool

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnprocessableEntity, "CUSTOM_VALIDATION", "bad")
					return nil
				}, maniflex.AtPosition(maniflex.After))
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					svcCalled = true
					return next()
				})
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					dbCalled = true
					return next()
				})
			},
		})
		srv.POST("/users", map[string]any{
			"name": "X", "email": "x@x.com", "password": "s",
		}).AssertStatus(http.StatusUnprocessableEntity)
		if svcCalled {
			t.Error("Service must not run after Validate abort")
		}
		if dbCalled {
			t.Error("DB must not run after Validate abort")
		}
	})

	// ── Replace position ──────────────────────────────────────────────────────

	t.Run("replace_swaps_out_default_handler", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// Replace the DB step — return a hardcoded 418 instead
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusTeapot,
						Error:      &maniflex.APIError{Code: "IM_A_TEAPOT", Message: "replaced"},
					}
					return nil
				}, maniflex.AtPosition(maniflex.Replace))
			},
		})
		srv.GET("/users").AssertStatus(http.StatusTeapot)
	})

	// ── ForModel scoping ──────────────────────────────────────────────────────

	t.Run("for_model_middleware_only_fires_on_targeted_model", func(t *testing.T) {
		t.Parallel()
		var postCount, userCount int

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					postCount++
					return next()
				}, maniflex.ForModel("Post"))
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					userCount++
					return next()
				}, maniflex.ForModel("User"))
			},
		})

		uid := srv.MustID(srv.CreateUser("U", "u@x.com", "viewer"))
		srv.MustID(srv.CreatePost("P", "draft", uid))

		if postCount != 1 {
			t.Errorf("Post middleware fired %d times, want 1", postCount)
		}
		if userCount != 1 {
			t.Errorf("User middleware fired %d times, want 1", userCount)
		}
	})

	t.Run("for_model_middleware_does_not_fire_on_other_models", func(t *testing.T) {
		t.Parallel()
		postMiddlewareFired := false

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					postMiddlewareFired = true
					return next()
				}, maniflex.ForModel("Post"))
			},
		})
		srv.CreateUser("U", "u@x.com", "viewer") // User request, not Post
		if postMiddlewareFired {
			t.Error("Post middleware must not fire on User requests")
		}
	})

	// ── ForOperation scoping ──────────────────────────────────────────────────

	t.Run("for_operation_middleware_fires_only_on_create", func(t *testing.T) {
		t.Parallel()
		var createCount, listCount int

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					createCount++
					return next()
				}, maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.MustID(srv.CreateUser("U", "u@x.com", "viewer"))
		srv.GET("/users")
		_ = listCount
		if createCount != 1 {
			t.Errorf("create middleware fired %d times, want 1", createCount)
		}
	})

	t.Run("for_operation_multiple_ops_only_fires_on_matching", func(t *testing.T) {
		t.Parallel()
		var callOps []string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					callOps = append(callOps, string(ctx.Operation))
					mu.Unlock()
					return next()
				}, maniflex.ForOperation(maniflex.OpCreate, maniflex.OpDelete))
			},
		})
		id := srv.MustID(srv.CreateUser("U", "u@x.com", "viewer"))
		srv.GET("/users")                                    // list — should not fire
		srv.GET("/users/" + id)                              // read — should not fire
		srv.PATCH("/users/"+id, map[string]any{"name": "X"}) // update — should not fire
		srv.DELETE("/users/" + id)                           // delete — should fire

		mu.Lock()
		ops := make([]string, len(callOps))
		copy(ops, callOps)
		mu.Unlock()

		testutil.AssertEqual(t, "fired ops", len(ops), 2) // create + delete
		testutil.AssertContains(t, "create fired", ops, "create")
		testutil.AssertContains(t, "delete fired", ops, "delete")
	})

	// ── Middleware mutation of context ────────────────────────────────────────

	t.Run("service_middleware_can_mutate_parsed_body", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					// Inject a role even though client didn't send one.
					// SetField writes through to both ParsedBody and the record.
					ctx.SetField("role", "editor")
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users", map[string]any{
			"name": "Injected", "email": "inj@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "injected role",
			testutil.Field(t, resp.Data(), "role"), "editor")
	})

	t.Run("after_response_middleware_can_read_dbresult", func(t *testing.T) {
		t.Parallel()
		var capturedID string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					if m, ok := ctx.DBResult.(map[string]any); ok {
						mu.Lock()
						capturedID, _ = m["id"].(string)
						mu.Unlock()
					}
					return nil
				}, maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		resp := srv.POST("/users", map[string]any{
			"name": "Captured", "email": "cap@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)

		mu.Lock()
		captured := capturedID
		mu.Unlock()

		testutil.AssertNotEmpty(t, "captured id", captured)
		testutil.AssertEqual(t, "captured matches response", captured, resp.ID())
	})

	// ── ctx.Set/Get across steps ──────────────────────────────────────────────

	t.Run("ctx_set_value_readable_in_later_step", func(t *testing.T) {
		t.Parallel()
		var receivedVal string

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Set("my_key", "my_value")
					return next()
				})
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if v, ok := ctx.Get("my_key"); ok {
						receivedVal, _ = v.(string)
					}
					return next()
				})
			},
		})
		srv.GET("/users")
		testutil.AssertEqual(t, "ctx value propagated", receivedVal, "my_value")
	})

	// ── OpenAPI pipeline ──────────────────────────────────────────────────────

	t.Run("openapi_auth_middleware_can_block_spec_access", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.OpenAPI.Auth.Register(func(ctx *maniflex.OpenAPIContext, next func() error) error {
					ctx.Abort(http.StatusForbidden, "FORBIDDEN", "spec is private")
					return nil
				})
			},
		})
		srv.GET("/openapi.json").AssertStatus(http.StatusForbidden)
	})

	t.Run("openapi_generate_after_middleware_can_mutate_spec", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.OpenAPI.Generate.Register(func(ctx *maniflex.OpenAPIContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					if ctx.Spec != nil {
						ctx.Spec.Info.Title = "Mutated Title"
					}
					return nil
				}, maniflex.After)
			},
		})
		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			info, _ := body["info"].(map[string]any)
			testutil.AssertEqual(t, "mutated title", info["title"], "Mutated Title")
		})
	})

	t.Run("openapi_generate_replace_middleware_serves_custom_spec", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.OpenAPI.Generate.Register(func(ctx *maniflex.OpenAPIContext, next func() error) error {
					ctx.Spec = &maniflex.OpenAPISpec{
						OpenAPI: "3.1.0",
						Info:    maniflex.OpenAPIInfo{Title: "Custom", Version: "99.0"},
					}
					return next() // still call next so Response step serialises it
				}, maniflex.Replace)
			},
		})
		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			info, _ := body["info"].(map[string]any)
			testutil.AssertEqual(t, "custom title", info["title"], "Custom")
			testutil.AssertEqual(t, "custom version", info["version"], "99.0")
		})
	})
}
