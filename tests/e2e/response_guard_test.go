package e2e

// The Response step used to panic on ctx.DBResult shapes a Replace middleware can
// plausibly produce: a ListResult with no Query (nil deref), one with a zero Limit
// (integer divide-by-zero in the page count), or a value that isn't a record at
// all (reflect.Value.Elem on a non-pointer). Each surfaced as a 500 PANIC on
// every request to the route (BUG-13).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// replaceDBStep swaps the DB step for one that sets ctx.DBResult to whatever the
// test wants to hand the Response step.
func replaceDBStep(t *testing.T, op maniflex.Operation, result func() any) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.DBResult = result()
					return next()
				},
				maniflex.ForModel("widget"),
				maniflex.ForOperation(op),
				maniflex.AtPosition(maniflex.Replace),
			)
		},
	})
}

// The audit's scenario: a Replace list step that sets only Items. Query is nil,
// so the page count divided by a zero Limit.
func TestResponseGuard_ListResultWithoutQuery(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return &maniflex.ListResult{
			Items: []any{&widget{Name: "a", Qty: 1}},
			Total: 1,
		}
	})

	resp := srv.GET("/widgets").AssertStatus(http.StatusOK)
	if n := len(resp.DataList()); n != 1 {
		t.Fatalf("got %d items, want 1", n)
	}
	// The pagination meta is filled from defaults rather than exploding.
	meta := resp.Meta()
	if meta["limit"] != float64(20) {
		t.Errorf("limit = %v, want the default 20", meta["limit"])
	}
	if meta["page"] != float64(1) {
		t.Errorf("page = %v, want 1", meta["page"])
	}
	if meta["pages"] != float64(1) {
		t.Errorf("pages = %v, want 1", meta["pages"])
	}
}

// Same shape, but with a Query whose Limit is left at its zero value.
func TestResponseGuard_ListResultWithZeroLimit(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return &maniflex.ListResult{
			Items: []any{&widget{Name: "a", Qty: 1}, &widget{Name: "b", Qty: 2}},
			Total: 2,
			Query: &maniflex.QueryParams{}, // Limit and Page both zero
		}
	})

	resp := srv.GET("/widgets").AssertStatus(http.StatusOK)
	if n := len(resp.DataList()); n != 2 {
		t.Fatalf("got %d items, want 2", n)
	}
	if got := resp.Meta()["pages"]; got != float64(1) {
		t.Errorf("pages = %v, want 1", got)
	}
}

// A DBResult of the wrong shape entirely is a bug in the middleware, not a panic
// in the framework: report it as a 500 that says what was expected.
func TestResponseGuard_ListResultWrongType(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return map[string]any{"not": "a list result"}
	})

	resp := srv.GET("/widgets")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}

// The read path has the same hazard: marshalRecord reflects into the value, so a
// non-pointer DBResult panicked inside reflect.Value.Elem.
func TestResponseGuard_ReadResultNotARecord(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpRead, func() any {
		return widget{Name: "by-value", Qty: 1} // a value, not a *widget
	})

	resp := srv.GET("/widgets/some-id")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}
