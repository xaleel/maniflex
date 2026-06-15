package e2e

// Phase 5 / T5.2 PoD: typed cross-model CRUD helpers (Create/Read/List/Update/
// Delete[T]) exchange *T, no map[string]any. Driven from an action handler that
// holds a ServerContext.

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func TestTyped_CRUDHelpers(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/widget-crud",
				Handler: func(ctx *maniflex.ServerContext) error {
					// Create
					created, err := maniflex.Create(ctx, &widget{Name: "alpha", Qty: 3})
					if err != nil {
						return err
					}
					if created.ID == "" || created.Name != "alpha" || created.Qty != 3 {
						ctx.Abort(http.StatusInternalServerError, "CREATE", "create wrong")
						return nil
					}
					id := created.ID

					// Read (fully-populated struct fields)
					got, err := maniflex.Read[widget](ctx, id)
					if err != nil {
						return err
					}
					if got.Name != "alpha" || got.Qty != 3 {
						ctx.Abort(http.StatusInternalServerError, "READ", "read wrong")
						return nil
					}

					// List
					all, err := maniflex.List[widget](ctx, nil)
					if err != nil {
						return err
					}
					if len(all) != 1 || all[0].Qty != 3 {
						ctx.Abort(http.StatusInternalServerError, "LIST", "list wrong")
						return nil
					}

					// Update (full record)
					got.Qty = 9
					upd, err := maniflex.Update(ctx, id, got)
					if err != nil {
						return err
					}
					if upd.Qty != 9 {
						ctx.Abort(http.StatusInternalServerError, "UPDATE", "update wrong")
						return nil
					}

					// Delete
					if err := maniflex.Delete[widget](ctx, id); err != nil {
						return err
					}
					if _, err := maniflex.Read[widget](ctx, id); err != maniflex.ErrNotFound {
						ctx.Abort(http.StatusInternalServerError, "DELETE", "expected ErrNotFound")
						return nil
					}

					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	srv.POST("/widget-crud", nil).AssertStatus(http.StatusOK)
}
