package e2e

// The HTTP path rejects an empty in/not_in list at parse time (see filter_test.go).
// A FilterExpr built by hand in Go — the typed List/QueryParams shape used by
// actions, tenancy middleware, and ingestion code — never goes near the parser,
// so the adapter must not emit "col IN ()" either. An empty IN matches nothing,
// an empty NOT IN matches everything (BUG-7).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_List_EmptyInFilterIsNotASyntaxError(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/widget-empty-in",
				Handler: func(ctx *maniflex.ServerContext) error {
					for _, n := range []string{"a", "b", "c"} {
						if _, err := maniflex.Create(ctx, &widget{Name: n, Qty: 1}); err != nil {
							return err
						}
					}

					// Empty IN: no syntax error, and it matches nothing.
					in, err := maniflex.List[widget](ctx, &maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "name", Operator: maniflex.OpIn, Value: "", Group: -1},
						},
					})
					if err != nil {
						return err
					}
					if len(in) != 0 {
						ctx.Abort(http.StatusInternalServerError, "EMPTY_IN",
							"empty IN must match no rows")
						return nil
					}

					// Empty NOT IN: excludes nothing, so every row comes back.
					notIn, err := maniflex.List[widget](ctx, &maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "name", Operator: maniflex.OpNotIn, Value: "", Group: -1},
						},
					})
					if err != nil {
						return err
					}
					if len(notIn) != 3 {
						ctx.Abort(http.StatusInternalServerError, "EMPTY_NOT_IN",
							"empty NOT IN must match every row")
						return nil
					}

					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	srv.POST("/widget-empty-in", nil).AssertStatus(http.StatusOK)
}
