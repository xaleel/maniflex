package e2e

// Phase 5 / T5.2 (typed Batch): the generic CRUD helpers route through ctx.Tx,
// and maniflex.Batch sets ctx.Tx for the duration of its callback — so typed
// Create/Read/List inside a Batch participate in its transaction (commit on
// success, rollback on error) with no map[string]any.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_BatchTransaction(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			// Rollback path: create two widgets, then fail → both undone.
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/batch-fail",
				Handler: func(ctx *maniflex.ServerContext) error {
					err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
						if _, e := maniflex.Create(ctx, &widget{Name: "a", Qty: 1}); e != nil {
							return e
						}
						if _, e := maniflex.Create(ctx, &widget{Name: "b", Qty: 2}); e != nil {
							return e
						}
						return errors.New("boom") // force rollback
					})
					if err == nil {
						ctx.Abort(http.StatusInternalServerError, "X", "expected batch error")
						return nil
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
			// Commit path: create two widgets, succeed → both persist.
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/batch-ok",
				Handler: func(ctx *maniflex.ServerContext) error {
					return maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
						if _, e := maniflex.Create(ctx, &widget{Name: "c", Qty: 3}); e != nil {
							return e
						}
						_, e := maniflex.Create(ctx, &widget{Name: "d", Qty: 4})
						return e
					})
				},
			})
			// Counter: list via the typed helper.
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/batch-count",
				Handler: func(ctx *maniflex.ServerContext) error {
					all, err := maniflex.List[widget](ctx, nil)
					if err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"count": len(all)},
					}
					return nil
				},
			})
		},
	})

	srv.POST("/batch-fail", nil).AssertStatus(http.StatusOK)
	srv.GET("/batch-count").
		AssertStatus(http.StatusOK).
		AssertJSON(func(body map[string]any) {
			if c := body["data"].(map[string]any)["count"]; c != float64(0) {
				t.Errorf("after rollback count = %v, want 0", c)
			}
		})

	srv.POST("/batch-ok", nil).AssertStatus(http.StatusOK)
	srv.GET("/batch-count").
		AssertStatus(http.StatusOK).
		AssertJSON(func(body map[string]any) {
			if c := body["data"].(map[string]any)["count"]; c != float64(2) {
				t.Errorf("after commit count = %v, want 2", c)
			}
		})
}
