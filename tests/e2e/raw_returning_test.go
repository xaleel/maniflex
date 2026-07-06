package e2e

// Regression (analytics svc #2): `UPDATE … RETURNING` / `INSERT … RETURNING` run
// through ctx.RawQuery used to be routed to ExecContext (query-vs-exec was a
// select-prefix sniff), so the returned rows were silently discarded. RETURNING
// data-modifying statements must now yield their rows on both the autocommit and
// the in-transaction paths.

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestRawQuery_Returning(t *testing.T) {
	t.Parallel()

	t.Run("update_returning_autocommit_yields_rows", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.RawQuery(
						"UPDATE users SET role = ? WHERE name = ? RETURNING id, role",
						"editor", "Dana")
					if err != nil {
						return fmt.Errorf("RawQuery RETURNING: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Dana", "rqr1@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("UPDATE ... RETURNING: want 1 row, got %d", len(rows))
		}
		if rows[0]["role"] != "editor" {
			t.Errorf("returned role: got %v, want editor", rows[0]["role"])
		}
	})

	t.Run("update_returning_in_tx_yields_rows", func(t *testing.T) {
		t.Parallel()
		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return fmt.Errorf("BeginTx: %w", err)
					}
					ctx.Tx = tx
					rows, err := ctx.RawQuery(
						"UPDATE users SET role = ? WHERE name = ? RETURNING id, role",
						"admin", "Erin")
					if err != nil {
						_ = tx.Rollback()
						ctx.Tx = nil
						return fmt.Errorf("tx RawQuery RETURNING: %w", err)
					}
					if err := tx.Commit(); err != nil {
						ctx.Tx = nil
						return fmt.Errorf("commit: %w", err)
					}
					ctx.Tx = nil
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.CreateUser("Erin", "rqr2@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("tx UPDATE ... RETURNING: want 1 row, got %d", len(rows))
		}
		if rows[0]["role"] != "admin" {
			t.Errorf("returned role: got %v, want admin", rows[0]["role"])
		}
	})
}
