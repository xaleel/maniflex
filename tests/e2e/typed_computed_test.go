package e2e

// Phase 4 / T4.5: typed computed fields. maniflex.AddComputedField[T] registers
// a derived field whose callback receives the record as *T. Works on read, list,
// and create echo (where the row is re-read via the scanStruct path).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_ComputedField(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			if err := maniflex.AddComputedField(s, "widget", "doubled",
				func(ctx *maniflex.ServerContext, w *widget) (any, error) {
					return w.Qty * 2, nil
				}); err != nil {
				t.Fatalf("AddComputedField: %v", err)
			}
		},
	})

	// Create echo carries the computed field (record re-read as *T).
	created := srv.POST("/widgets", map[string]any{"name": "a", "qty": 7})
	created.AssertStatus(http.StatusCreated)
	if got := created.Data()["doubled"]; got != float64(14) {
		t.Errorf("create echo doubled = %v, want 14", got)
	}
	id := created.ID()

	// Read.
	srv.GET("/widgets/" + id).AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		data := body["data"].(map[string]any)
		if data["doubled"] != float64(14) {
			t.Errorf("read doubled = %v, want 14", data["doubled"])
		}
	})

	// List.
	srv.GET("/widgets").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		items := body["data"].([]any)
		if len(items) != 1 {
			t.Fatalf("want 1 item, got %d", len(items))
		}
		if items[0].(map[string]any)["doubled"] != float64(14) {
			t.Errorf("list doubled = %v, want 14", items[0].(map[string]any)["doubled"])
		}
	})
}
