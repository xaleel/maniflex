package e2e

// P0 #5 — maniflex.Update[T] is a FULL-record write (every column except id),
// NOT a PATCH. Any field left unset on the passed *T is overwritten to its zero
// value. This pins the footgun so callers don't mistake the typed helper for a
// partial update; partial writes go through the HTTP PATCH pipeline (which honors
// presence — see typed_presence_test.go).

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func TestTypedUpdate_FullRecordOverwritesOmittedFields(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/widget-overwrite",
				Handler: func(ctx *maniflex.ServerContext) error {
					created, err := maniflex.Create(ctx, &widget{Name: "orig", Qty: 5})
					if err != nil {
						return err
					}
					// Set only Name; leave Qty at its zero value. Update[T] writes
					// the whole record, so Qty must be overwritten to 0.
					if _, err := maniflex.Update(ctx, created.ID, &widget{Name: "renamed"}); err != nil {
						return err
					}
					got, err := maniflex.Read[widget](ctx, created.ID)
					if err != nil {
						return err
					}
					if got.Name != "renamed" {
						ctx.Abort(http.StatusInternalServerError, "NAME", "name not updated")
						return nil
					}
					if got.Qty != 0 {
						ctx.Abort(http.StatusInternalServerError, "QTY",
							"Update[T] must overwrite omitted Qty to 0 (full-record), not preserve 5")
						return nil
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})
	srv.POST("/widget-overwrite", nil).AssertStatus(http.StatusOK)
}
