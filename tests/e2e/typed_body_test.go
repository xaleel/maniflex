package e2e

// Phase 5 / T5.4: ctx.SetField writes through to both ParsedBody (the write
// source) and the typed ctx.Record, so a value injected by one middleware both
// persists and is visible to a later typed (For[T]) middleware.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_SetFieldWriteThrough(t *testing.T) {
	var seenViaFor int

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			// 1) inject a field value via the write-through setter
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.SetField("qty", 99)
					return next()
				},
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpCreate),
			)
			// 2) a later typed middleware sees it on ctx.Record
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, w *widget) error {
					seenViaFor = w.Qty
					return nil
				}),
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	resp := srv.POST("/widgets", map[string]any{"name": "x", "qty": 5})
	resp.AssertStatus(http.StatusCreated)

	// Persisted value reflects the write-through to ParsedBody.
	if got := resp.Data()["qty"]; got != float64(99) {
		t.Errorf("persisted qty = %v, want 99 (SetField → ParsedBody → write)", got)
	}
	// The typed record was kept in sync, so For[widget] saw it.
	if seenViaFor != 99 {
		t.Errorf("For[widget].Qty = %d, want 99 (SetField → ctx.Record)", seenViaFor)
	}
}
