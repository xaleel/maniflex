package e2e

// Phase 5 / T5.1 PoD: a middleware reads the request body as a concrete *T via
// maniflex.Handle / For / Bind — zero map[string]any / any.

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

type widget struct {
	maniflex.BaseModel
	Name string `json:"name"`
	Qty  int    `json:"qty"`
}

func TestTyped_HandleReadsTypedBody(t *testing.T) {
	var seenName string
	var seenQty int

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, w *widget) error {
					seenName, seenQty = w.Name, w.Qty
					if w.Qty < 0 {
						ctx.Abort(http.StatusUnprocessableEntity, "BAD_QTY", "qty must be >= 0")
					}
					return nil
				}),
				maniflex.ForModel("widget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	// Valid body → the handler sees the typed struct.
	srv.POST("/widgets", map[string]any{"name": "gizmo", "qty": 5}).
		AssertStatus(http.StatusCreated)
	if seenName != "gizmo" || seenQty != 5 {
		t.Fatalf("Handle did not receive typed body: name=%q qty=%d", seenName, seenQty)
	}

	// Invalid body → the typed handler aborts.
	srv.POST("/widgets", map[string]any{"name": "bad", "qty": -1}).
		AssertStatus(http.StatusUnprocessableEntity)
}

func TestTyped_ForAndBind(t *testing.T) {
	var forOK, bindOK bool

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					if w, ok := maniflex.For[widget](ctx); ok && w.Name == "viaFor" {
						forOK = true
					}
					if w, err := maniflex.Bind[widget](ctx); err == nil && w.Name == "viaFor" {
						bindOK = true
					}
					return next()
				},
				maniflex.ForModel("widget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	srv.POST("/widgets", map[string]any{"name": "viaFor", "qty": 1}).
		AssertStatus(http.StatusCreated)
	if !forOK || !bindOK {
		t.Fatalf("For/Bind did not yield the typed record: for=%v bind=%v", forOK, bindOK)
	}
}
