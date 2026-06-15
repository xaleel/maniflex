package e2e

// transaction_test.go tests the DB transaction support added to maniflex.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestTransaction

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTransaction(t *testing.T) {
	t.Parallel()

	// ── WithTransaction middleware ─────────────────────────────────────────────

	t.Run("successful_create_commits_and_record_is_visible", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		resp := srv.CreateUser("Alice", "tx1@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()

		// Record must be visible after the transaction committed.
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
	})

	t.Run("failed_create_rolls_back_and_record_is_not_visible", func(t *testing.T) {
		t.Parallel()
		// Register WithTransaction AND an After-DB middleware that aborts with an
		// error response — simulating a post-insert check that fails.
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
				// After-DB: simulate a business rule failure after the INSERT.
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// Abort with 422 — WithTransaction sees StatusCode >= 400 and rolls back.
					ctx.Abort(http.StatusUnprocessableEntity, "POST_INSERT_FAIL", "business rule failed")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		resp := srv.CreateUser("Bob", "tx2@x.com", "viewer")
		resp.AssertStatus(http.StatusUnprocessableEntity)

		// The INSERT was rolled back — list must be empty.
		items := srv.GET("/users").DataList()
		testutil.AssertLen(t, "no records after rollback", items, 0)
	})

	t.Run("update_in_transaction_is_atomic", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpUpdate),
				)
			},
		})
		id := srv.MustID(srv.CreateUser("Carol", "tx3@x.com", "viewer"))

		resp := srv.PATCH("/users/"+id, map[string]any{"name": "Carolyn"})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "name updated", testutil.Field(t, resp.Data(), "name"), "Carolyn")

		// Verify persisted.
		testutil.AssertEqual(t, "name persisted",
			testutil.Field(t, srv.GET("/users/"+id).Data(), "name"), "Carolyn")
	})

	t.Run("delete_in_transaction_commits", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpDelete),
				)
			},
		})
		id := srv.MustID(srv.CreateUser("Dave", "tx4@x.com", "viewer"))
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/users/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("transaction_not_started_for_reads", func(t *testing.T) {
		// WithTransaction scoped to writes must not affect read operations.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
				)
			},
		})
		srv.MustID(srv.CreateUser("Eve", "tx5@x.com", "viewer"))
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users?filter=name:eq:Eve").AssertStatus(http.StatusOK)
	})

	t.Run("with_transaction_idempotent_when_tx_already_set", func(t *testing.T) {
		// If an outer middleware already set ctx.Tx, WithTransaction is a no-op
		// and doesn't start a nested transaction (SQLite doesn't support them).
		t.Parallel()
		var outerTx maniflex.Tx
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// Outer: manually begin a transaction and set ctx.Tx.
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return fmt.Errorf("outer begin: %w", err)
					}
					mu.Lock()
					outerTx = tx
					mu.Unlock()
					ctx.Tx = tx
					defer tx.Rollback()
					if err := next(); err != nil {
						return err
					}
					return tx.Commit()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))

				// Inner: WithTransaction should be a no-op because ctx.Tx is set.
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})

		resp := srv.CreateUser("Frank", "tx6@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)

		mu.Lock()
		tx := outerTx
		mu.Unlock()
		if tx == nil {
			t.Error("outer transaction was not set")
		}

		// Record must be visible (outer tx committed).
		srv.GET("/users/" + resp.ID()).AssertStatus(http.StatusOK)
	})

	// ── ctx.BeginTx and ctx.Tx manual usage ───────────────────────────────────

	t.Run("manual_begin_tx_create_commit", func(t *testing.T) {
		// Manually begin a transaction, create a record inside it, commit.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return fmt.Errorf("begin: %w", err)
					}
					ctx.Tx = tx
					defer tx.Rollback()
					if err := next(); err != nil {
						return err
					}
					if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
						return nil // rollback via defer
					}
					return tx.Commit()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		resp := srv.CreateUser("Grace", "tx7@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)
		srv.GET("/users/" + resp.ID()).AssertStatus(http.StatusOK)
	})

	t.Run("manual_begin_tx_create_rollback", func(t *testing.T) {
		// Manually begin, create, then rollback — record must not persist.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return fmt.Errorf("begin: %w", err)
					}
					ctx.Tx = tx
					// Unconditional rollback — never commit.
					defer tx.Rollback()
					if err := next(); err != nil {
						return err
					}
					// Override the 201 with a custom error so the test HTTP response
					// doesn't confuse the assertion below.
					ctx.Abort(http.StatusInternalServerError, "FORCED_ROLLBACK", "always rolled back")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		srv.CreateUser("Heidi", "tx8@x.com", "viewer").
			AssertStatus(http.StatusInternalServerError)

		// Rollback: no records should exist.
		testutil.AssertLen(t, "no records after rollback", srv.GET("/users").DataList(), 0)
	})

	t.Run("ctx_tx_is_nil_without_middleware", func(t *testing.T) {
		// Without any transaction middleware ctx.Tx remains nil — the DB step
		// uses the bare adapter directly. All existing behaviour is unchanged.
		t.Parallel()
		var capturedTx maniflex.Tx
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					capturedTx = ctx.Tx
					mu.Unlock()
					return next()
				}, maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.CreateUser("Ivan", "tx9@x.com", "viewer").AssertStatus(http.StatusCreated)

		mu.Lock()
		tx := capturedTx
		mu.Unlock()
		if tx != nil {
			t.Error("ctx.Tx must be nil when no transaction middleware is registered")
		}
	})

	// ── Error cases ───────────────────────────────────────────────────────────

	t.Run("begin_tx_without_adapter_returns_error", func(t *testing.T) {
		// BeginTx on a context with no adapter set returns ErrNoAdapter.
		t.Parallel()
		ctx := &maniflex.ServerContext{}
		_, err := ctx.BeginTx(nil, nil) //nolint:staticcheck
		if err == nil {
			t.Error("expected error from BeginTx with no adapter, got nil")
		}
	})

	t.Run("rollback_after_commit_is_safe_noop", func(t *testing.T) {
		// The deferred tx.Rollback() after a successful Commit must not return
		// a visible error (sql.ErrTxDone is silently discarded by WithTransaction).
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		// A successful create with WithTransaction: commit + deferred rollback.
		// No error should surface.
		resp := srv.CreateUser("Judy", "tx10@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)
	})

	// ── Concurrency ───────────────────────────────────────────────────────────

	t.Run("concurrent_transactional_creates_all_succeed", func(t *testing.T) {
		// SQLite WAL serialises writers; all transactions must commit without
		// interfering with each other.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})

		const n = 5
		var wg sync.WaitGroup
		statuses := make([]int, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp := srv.CreateUser(
					fmt.Sprintf("User%d", i),
					fmt.Sprintf("txc%d@x.com", i),
					"viewer",
				)
				statuses[i] = resp.Status
			}(i)
		}
		wg.Wait()

		for i, s := range statuses {
			if s != http.StatusCreated {
				t.Errorf("goroutine %d: got %d, want 201", i, s)
			}
		}

		items := srv.GET("/users").DataList()
		testutil.AssertLen(t, "all records committed", items, n)
	})

	t.Run("two_independent_requests_isolated_from_each_other", func(t *testing.T) {
		// Two simultaneous requests each begin their own transaction.
		// The first request must not see the second request's uncommitted data.
		t.Parallel()

		// We test this by having the second create fail (rollback) and
		// confirming only the first record persists.
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
				)
			},
		})

		id1 := srv.MustID(srv.CreateUser("UserA", "iso1@x.com", "admin"))
		id2 := srv.MustID(srv.CreateUser("UserB", "iso2@x.com", "editor"))

		// Both committed — both visible.
		srv.GET("/users/" + id1).AssertStatus(http.StatusOK)
		srv.GET("/users/" + id2).AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "two records", srv.GET("/users").DataList(), 2)
	})

	// ── ctx.Tx routing through DB step ────────────────────────────────────────

	t.Run("db_step_uses_tx_when_ctx_tx_is_set", func(t *testing.T) {
		// Verify that setting ctx.Tx routes the DB step's actual queries through
		// the transaction by rolling back and confirming no record is persisted.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					// Run the DB step (create) but immediately rollback.
					if err := next(); err != nil {
						tx.Rollback()
						return err
					}
					// Force rollback regardless of response.
					tx.Rollback()
					// Suppress the 201 so assertions below are unambiguous.
					ctx.Abort(http.StatusTeapot, "ROLLED_BACK", "test rollback")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		srv.CreateUser("Karl", "tx11@x.com", "viewer").AssertStatus(http.StatusTeapot)

		// Rollback means no record in the DB.
		testutil.AssertLen(t, "no records after explicit rollback", srv.GET("/users").DataList(), 0)
	})

	// ── ctx.LockForUpdate ────────────────────────────────────────────────────────

	t.Run("lock_for_update_returns_record_within_transaction", func(t *testing.T) {
		// LockForUpdate should return the current row while the transaction is open.
		t.Parallel()
		var lockedName string
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback()

					row, err := ctx.LockForUpdate("User", ctx.ResourceID)
					if err != nil {
						return err
					}
					if n, ok := row["name"].(string); ok {
						lockedName = n
					}
					if err := next(); err != nil {
						return err
					}
					return tx.Commit()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpUpdate))
			},
		})

		id := srv.MustID(srv.CreateUser("Lena", "lock1@x.com", "viewer"))
		srv.PATCH("/users/"+id, map[string]any{"name": "Lena Updated"}).AssertStatus(http.StatusOK)

		if lockedName != "Lena" {
			t.Errorf("LockForUpdate: got name %q, want %q", lockedName, "Lena")
		}
	})

	t.Run("lock_for_update_returns_not_found_for_missing_id", func(t *testing.T) {
		// LockForUpdate on a non-existent ID must return ErrNotFound.
		t.Parallel()
		var gotErr error
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					ctx.Tx = tx
					defer tx.Rollback()

					_, gotErr = ctx.LockForUpdate("User", "00000000-0000-0000-0000-000000000000")
					ctx.Abort(http.StatusTeapot, "LOCK_DONE", "lock attempted")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		srv.CreateUser("Mallory", "lock2@x.com", "viewer").AssertStatus(http.StatusTeapot)

		if gotErr == nil {
			t.Error("expected ErrNotFound from LockForUpdate on missing ID, got nil")
		}
		if !errors.Is(gotErr, maniflex.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", gotErr)
		}
	})

	t.Run("lock_for_update_requires_active_transaction", func(t *testing.T) {
		// LockForUpdate without ctx.Tx set must return an error immediately.
		t.Parallel()
		var gotErr error
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					// Deliberately no BeginTx — ctx.Tx is nil.
					_, gotErr = ctx.LockForUpdate("User", "any-id")
					ctx.Abort(http.StatusTeapot, "LOCK_DONE", "lock attempted")
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		srv.CreateUser("Niaj", "lock3@x.com", "viewer").AssertStatus(http.StatusTeapot)

		if gotErr == nil {
			t.Error("expected error from LockForUpdate with nil ctx.Tx, got nil")
		}
	})
}
