package e2e

// P1 #9 — For/Bind edge cases (the happy paths are in typed_api_test.go):
//   - For[Wrong] when a *Right is bound must return (nil, false), not a panic.
//   - Bind[Wrong] must return an error.
//   - For[T] on an operation with no request body (a read) must return false,
//     and Handle[T] must skip its callback.

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// otherThing is intentionally NOT registered — For/Bind only type-assert against
// ctx.Record, so a mismatching type parameter must simply not match.
type otherThing struct {
	maniflex.BaseModel
	Label string `json:"label" db:"label"`
}

func TestTypedFor_MismatchAndNoBody(t *testing.T) {
	var forMismatchFalse, bindMismatchErr, forNoBodyFalse, handleSkipped bool
	handleSkipped = true // set false if the typed handler ever runs on the read

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			// On widget create a *widget is bound; For/Bind[otherThing] must not match.
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					if _, ok := maniflex.For[otherThing](ctx); !ok {
						forMismatchFalse = true
					}
					if _, err := maniflex.Bind[otherThing](ctx); err != nil {
						bindMismatchErr = true
					}
					return next()
				},
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpCreate),
			)
			// On a list (no body), For[widget] must be false and Handle[widget] skip.
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					if _, ok := maniflex.For[widget](ctx); !ok {
						forNoBodyFalse = true
					}
					return next()
				},
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpList),
			)
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, _ *widget) error {
					handleSkipped = false // must NOT run: no body bound on a read
					return nil
				}),
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpList),
			)
		},
	})

	srv.POST("/widgets", map[string]any{"name": "x", "qty": 1}).AssertStatus(http.StatusCreated)
	if !forMismatchFalse {
		t.Error("For[otherThing] should be false when a *widget is bound (type mismatch)")
	}
	if !bindMismatchErr {
		t.Error("Bind[otherThing] should error when a *widget is bound")
	}

	srv.GET("/widgets").AssertStatus(http.StatusOK)
	if !forNoBodyFalse {
		t.Error("For[widget] should be false on a read with no body")
	}
	if !handleSkipped {
		t.Error("Handle[widget] callback should be skipped on a read with no body")
	}
}
