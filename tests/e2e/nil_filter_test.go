package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// A nil *FilterExpr is reachable only from Go — the URL parser never produces
// one — but it is easy to produce there. ctx.ViaFilter returns (nil, error) on
// every failure path, and its own doc warns that skipping such an error "would
// leave the request unscoped rather than refused"; a caller who ignores it
// appends exactly this nil, and the thing it was meant to be was a scope.
//
// So a nil is refused rather than skipped: silently ignoring it runs the query
// with a narrower scope than the code reads as applying (audit O1).
//
// Every entry point below panicked before this fix. The panic was recovered as
// a 500, which is why this is Low rather than a crash — but a panic is not a
// diagnosis, and the message named nothing the caller could act on.

func TestNilFilter_AggregateWhereRefused(t *testing.T) {
	t.Parallel()

	var aggErr, recErr error
	var aggPanic, recPanic any
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.Post{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/posts/nilfilter",
				Handler: func(ctx *maniflex.ServerContext) error {
					func() {
						defer func() { aggPanic = recover() }()
						_, aggErr = ctx.Aggregate("Post", maniflex.AggregateQuery{
							Select: []maniflex.AggregateField{{Op: maniflex.AggCount, Field: "id", As: "n"}},
							Where:  []*maniflex.FilterExpr{nil},
						})
					}()
					func() {
						defer func() { recPanic = recover() }()
						_, recErr = ctx.RecursiveQuery("Post", maniflex.RecursiveQuery{
							RootID:      "some-id",
							ParentField: "user_id",
							Where:       []*maniflex.FilterExpr{nil},
						})
					}()
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})
	srv.GET("/posts/nilfilter").AssertStatus(http.StatusOK)

	if aggPanic != nil {
		t.Errorf("ctx.Aggregate panicked on a nil filter: %v", aggPanic)
	}
	if aggErr == nil || !strings.Contains(aggErr.Error(), "nil filter") {
		t.Errorf("ctx.Aggregate error = %v, want one naming a nil filter", aggErr)
	}

	if recPanic != nil {
		t.Errorf("ctx.RecursiveQuery panicked on a nil filter: %v", recPanic)
	}
	if recErr == nil || !strings.Contains(recErr.Error(), "nil filter") {
		t.Errorf("ctx.RecursiveQuery error = %v, want one naming a nil filter", recErr)
	}
}

// The list path reaches the adapter's join builder, which dereferenced every
// filter to ask whether it was a relation filter. This is the entry point the
// audit did not name, and the one a real application hits: a middleware that
// builds a filter conditionally and appends whatever it got.
func TestNilFilter_QueryFiltersRefused(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.Post{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Query != nil {
					ctx.Query.Filters = append(ctx.Query.Filters, nil)
				}
				return next()
			})
		},
	})

	resp := srv.GET("/posts")
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Status)
	}
	// A 500 either way — the point is which one. PANIC says the framework fell
	// over; INVALID_FILTER says it recognised the caller's mistake and named it,
	// which is the same code path a Go-built filter with a bad operator or an
	// unknown field already takes.
	body := string(resp.Body)
	if strings.Contains(body, `"PANIC"`) {
		t.Errorf("nil filter still panics through the list path: %s", body)
	}
	if !strings.Contains(body, "INVALID_FILTER") {
		t.Errorf("want an INVALID_FILTER diagnosis, got: %s", body)
	}
}
