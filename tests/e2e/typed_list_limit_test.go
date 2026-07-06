package e2e

// Regression: maniflex.List[T] with a non-nil *QueryParams whose Limit is left at
// its zero value used to issue LIMIT 0 and return zero rows (it only substituted
// the default limit when q == nil). A hand-built &QueryParams{Filters: …} is the
// common shape in actions/ingestion, so this looked like a filter/visibility bug.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_List_ZeroLimitReturnsRows(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/widget-list-limit",
				Handler: func(ctx *maniflex.ServerContext) error {
					for _, n := range []string{"a", "b", "c"} {
						if _, err := maniflex.Create(ctx, &widget{Name: n, Qty: 1}); err != nil {
							return err
						}
					}

					// Non-nil QueryParams with Limit unset (0): must NOT return zero rows.
					got, err := maniflex.List[widget](ctx, &maniflex.QueryParams{})
					if err != nil {
						return err
					}
					if len(got) != 3 {
						ctx.Abort(http.StatusInternalServerError, "LIST_ZERO_LIMIT",
							"expected 3 rows from &QueryParams{}, got a different count")
						return nil
					}

					// A filtered hand-built query with no explicit limit also returns matches.
					filtered, err := maniflex.List[widget](ctx, &maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{{Field: "name", Operator: maniflex.OpEq, Value: "b", Group: -1}},
					})
					if err != nil {
						return err
					}
					if len(filtered) != 1 || filtered[0].Name != "b" {
						ctx.Abort(http.StatusInternalServerError, "LIST_FILTERED", "filtered list wrong")
						return nil
					}

					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	srv.POST("/widget-list-limit", nil).AssertStatus(http.StatusOK)
}
