package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// TestBatch covers maniflex.Batch (3D.4) — atomic cross-model writes.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestBatch
func TestBatch(t *testing.T) {
	t.Parallel()

	// ── Happy path ────────────────────────────────────────────────────────────

	t.Run("multi_model_create_commits_atomically", func(t *testing.T) {
		// Create a Post and two Comments in a single Batch call.
		// All three records must be visible after the action returns.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-create/{userID}",
					Handler: func(ctx *maniflex.ServerContext) error {
						uid := ctx.URLParam("userID")
						var postID string

						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							post, err := b.Create("Post", map[string]any{
								"title":   "Batch Post",
								"body":    "created inside Batch",
								"status":  "draft",
								"user_id": uid,
							})
							if err != nil {
								return err
							}
							postID, _ = post["id"].(string)

							for i := range 2 {
								if _, err := b.Create("Comment", map[string]any{
									"body":    fmt.Sprintf("comment %d", i+1),
									"post_id": postID,
									"user_id": uid,
								}); err != nil {
									return err
								}
							}
							return nil
						})
						if err != nil {
							ctx.Abort(http.StatusInternalServerError, "BATCH_ERR", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{
							StatusCode: http.StatusCreated,
							Data:       map[string]any{"post_id": postID},
						}
						return nil
					},
				})
			},
		})

		uid := srv.MustID(srv.CreateUser("Alice", "batch1@x.com", "viewer"))
		data := srv.POST("/batch-create/"+uid, nil).AssertStatus(http.StatusCreated).Data()

		postID, _ := data["post_id"].(string)
		if postID == "" {
			t.Fatal("expected post_id in response")
		}

		srv.GET("/posts/" + postID).AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "two comments persisted",
			srv.GET("/comments?filter=post_id:eq:"+postID).DataList(), 2)
	})

	// ── Rollback on explicit error ─────────────────────────────────────────────

	t.Run("rollback_on_explicit_fn_error", func(t *testing.T) {
		// fn creates a Tag then returns an explicit error.
		// The Tag must not be persisted.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-explicit-error",
					Handler: func(ctx *maniflex.ServerContext) error {
						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							if _, err := b.Create("Tag", map[string]any{
								"name":  "should-not-persist",
								"color": "red",
							}); err != nil {
								return err
							}
							return fmt.Errorf("simulated failure")
						})
						if err != nil {
							ctx.Abort(http.StatusUnprocessableEntity, "BATCH_FAILED", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusCreated}
						return nil
					},
				})
			},
		})

		srv.POST("/batch-explicit-error", nil).AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertLen(t, "no tags after rollback", srv.GET("/tags").DataList(), 0)
	})

	// ── Rollback on ctx.Abort ─────────────────────────────────────────────────

	t.Run("rollback_on_ctx_abort", func(t *testing.T) {
		// fn creates a Tag, then calls ctx.Abort and returns nil — the pattern
		// used by pipeline middleware. Batch must detect the error response and
		// roll back rather than commit, and must return a non-nil error.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-ctx-abort",
					Handler: func(ctx *maniflex.ServerContext) error {
						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							if _, berr := b.Create("Tag", map[string]any{
								"name":  "abort-tag",
								"color": "blue",
							}); berr != nil {
								return berr
							}
							// Pipeline-style abort: set error response, return nil.
							ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "invalid state")
							return nil
						})
						// Batch must have returned non-nil error.
						if err == nil {
							ctx.Abort(http.StatusInternalServerError, "EXPECTED_ERR", "Batch should have returned an error")
							return nil
						}
						// ctx.Response already set by ctx.Abort inside fn; nothing to add.
						return nil
					},
				})
			},
		})

		srv.POST("/batch-ctx-abort", nil).AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertLen(t, "no tags after abort rollback", srv.GET("/tags").DataList(), 0)
	})

	// ── Constraint violation ──────────────────────────────────────────────────

	t.Run("constraint_violation_rolls_back_entire_batch", func(t *testing.T) {
		// Create one tag normally, then inside Batch:
		//   - create a new tag with a unique name
		//   - attempt to create another tag with the pre-existing unique name
		// The constraint error should roll back both inserts.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-constraint",
					Handler: func(ctx *maniflex.ServerContext) error {
						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							// This tag has a unique name — insert is fine.
							if _, berr := b.Create("Tag", map[string]any{
								"name":  "new-unique-tag",
								"color": "green",
							}); berr != nil {
								return berr
							}
							// Duplicate name — violates the unique constraint.
							_, berr := b.Create("Tag", map[string]any{
								"name":  "existing-tag",
								"color": "red",
							})
							return berr
						})
						if err != nil {
							ctx.Abort(http.StatusConflict, "BATCH_CONSTRAINT", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusCreated}
						return nil
					},
				})
			},
		})

		// Seed the conflicting tag before the action runs.
		srv.POST("/tags", map[string]any{"name": "existing-tag", "color": "yellow"}).
			AssertStatus(http.StatusCreated)

		srv.POST("/batch-constraint", nil).AssertStatus(http.StatusConflict)

		// Only the pre-seeded tag must remain — neither new-unique-tag nor a
		// duplicate existing-tag was committed.
		tags := srv.GET("/tags").DataList()
		testutil.AssertLen(t, "only original tag after rollback", tags, 1)
		testutil.AssertEqual(t, "original tag intact",
			testutil.Field(t, tags[0].(map[string]any), "name"), "existing-tag")
	})

	// ── All CRUD ops inside Batch ─────────────────────────────────────────────

	t.Run("all_crud_ops_visible_within_batch", func(t *testing.T) {
		// Inside a single Batch call: Create, Read, Update, Delete, and List
		// must all see each other's uncommitted writes.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-crud/{userID}",
					Handler: func(ctx *maniflex.ServerContext) error {
						uid := ctx.URLParam("userID")
						result := map[string]any{}

						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							// Create
							post, err := b.Create("Post", map[string]any{
								"title":   "CRUD Test",
								"body":    "initial",
								"status":  "draft",
								"user_id": uid,
							})
							if err != nil {
								return fmt.Errorf("create: %w", err)
							}
							pid, _ := post["id"].(string)

							// Read — must see the just-created row
							read, err := b.Read("Post", pid)
							if err != nil {
								return fmt.Errorf("read: %w", err)
							}
							result["read_title"] = read["title"]

							// Update
							updated, err := b.Update("Post", pid, map[string]any{"status": "published"})
							if err != nil {
								return fmt.Errorf("update: %w", err)
							}
							result["updated_status"] = updated["status"]

							// Create a comment to delete
							comment, err := b.Create("Comment", map[string]any{
								"body":    "to be deleted",
								"post_id": pid,
								"user_id": uid,
							})
							if err != nil {
								return fmt.Errorf("create comment: %w", err)
							}
							cid, _ := comment["id"].(string)

							// Delete
							if err := b.Delete("Comment", cid); err != nil {
								return fmt.Errorf("delete: %w", err)
							}

							// List — deleted comment must be absent
							comments, err := b.List("Comment", &maniflex.QueryParams{
								Filters: []*maniflex.FilterExpr{
									{Field: "post_id", Operator: maniflex.OpEq, Value: pid},
								},
								Page: 1, Limit: 20,
							})
							if err != nil {
								return fmt.Errorf("list: %w", err)
							}
							result["comment_count"] = len(comments)
							result["post_id"] = pid
							return nil
						})
						if err != nil {
							ctx.Abort(http.StatusInternalServerError, "BATCH_ERR", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: result}
						return nil
					},
				})
			},
		})

		uid := srv.MustID(srv.CreateUser("Bob", "batch5@x.com", "viewer"))
		data := srv.POST("/batch-crud/"+uid, nil).AssertStatus(http.StatusOK).Data()

		testutil.AssertEqual(t, "read sees created row", data["read_title"], "CRUD Test")
		testutil.AssertEqual(t, "update applied", data["updated_status"], "published")
		testutil.AssertEqual(t, "delete removed comment",
			int(data["comment_count"].(float64)), 0)

		// Verify committed state via REST.
		postID, _ := data["post_id"].(string)
		srv.GET("/posts/" + postID).AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "no comments after delete", srv.GET("/comments").DataList(), 0)
	})

	// ── Joins outer transaction ───────────────────────────────────────────────

	t.Run("joins_outer_tx_and_respects_its_rollback", func(t *testing.T) {
		// Action handler opens a transaction manually, sets ctx.Tx, then calls
		// Batch. Batch must join the outer tx without opening a new one or
		// committing. When the outer tx is rolled back, the Batch writes must
		// also disappear.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-join-outer",
					Handler: func(ctx *maniflex.ServerContext) error {
						// Open outer transaction manually.
						tx, err := ctx.BeginTx(ctx.Ctx, nil)
						if err != nil {
							return err
						}
						ctx.Tx = tx
						defer tx.Rollback() // always roll back — never commit in this test

						// Batch must join the outer tx (ctx.Tx != nil).
						batchErr := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							_, berr := b.Create("Tag", map[string]any{
								"name":  "joined-outer-tx",
								"color": "purple",
							})
							return berr
						})
						if batchErr != nil {
							ctx.Abort(http.StatusInternalServerError, "BATCH_ERR", batchErr.Error())
							return nil
						}

						// Outer tx rolls back via defer.
						// Signal the test that we reached this point without errors.
						ctx.Response = &maniflex.APIResponse{
							StatusCode: http.StatusOK,
							Data:       map[string]any{"batch_ok": true},
						}
						return nil
					},
				})
			},
		})

		data := srv.POST("/batch-join-outer", nil).AssertStatus(http.StatusOK).Data()
		if data["batch_ok"] != true {
			t.Errorf("batch_ok: got %v, want true", data["batch_ok"])
		}

		// Outer tx rolled back — the tag must not exist.
		testutil.AssertLen(t, "no tags after outer rollback", srv.GET("/tags").DataList(), 0)
	})

	// ── Unknown model ─────────────────────────────────────────────────────────

	t.Run("unknown_model_returns_descriptive_error", func(t *testing.T) {
		// b.Create on an unregistered model name must return a clear error,
		// not panic, and the whole batch must roll back.
		t.Parallel()

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "POST",
					Path:   "/batch-unknown-model",
					Handler: func(ctx *maniflex.ServerContext) error {
						var batchErr error
						err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
							// First create succeeds.
							if _, berr := b.Create("Tag", map[string]any{
								"name": "real-tag", "color": "orange",
							}); berr != nil {
								return berr
							}
							// Unknown model must return an error, not panic.
							_, batchErr = b.Create("NoSuchModel", map[string]any{"x": 1})
							return batchErr
						})
						if err != nil {
							ctx.Abort(http.StatusBadRequest, "UNKNOWN_MODEL", err.Error())
							return nil
						}
						ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
						return nil
					},
				})
			},
		})

		srv.POST("/batch-unknown-model", nil).AssertStatus(http.StatusBadRequest)
		// The first Create (Tag) must have been rolled back too.
		testutil.AssertLen(t, "no tags after unknown model rollback", srv.GET("/tags").DataList(), 0)
	})
}
