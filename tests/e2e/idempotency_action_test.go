package e2e

// P1-8 — idempotency.Middleware in an ActionConfig.Middleware list used to hash an
// empty ctx.RawBody, because an action's pipeline skips the Deserialize step that
// populates it. So the "same key, different body -> 422" guard never fired: a
// client could reuse a key with a different body and silently replay the first
// response. The middleware now reads the body itself (ctx.EnsureRawBody) and
// restores it for the handler.

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/idempotency"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestIdempotency_InActionMiddlewareList(t *testing.T) {
	t.Parallel()

	store := maniflex.NewMemoryCache()
	var runs atomic.Int64

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/charge",
				Middleware: []maniflex.MiddlewareFunc{
					idempotency.Middleware(idempotency.Config{Store: store, TTL: time.Hour}),
				},
				Handler: func(ctx *maniflex.ServerContext) error {
					var req struct {
						Amount int `json:"amount"`
					}
					if err := ctx.BindJSON(&req); err != nil {
						return nil // ctx.Abort already called
					}
					n := runs.Add(1)
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"amount": req.Amount, "run": n},
					}
					return nil
				},
			})
		},
	})

	key := map[string]string{"Idempotency-Key": "charge-1"}

	// First call: the handler runs and sees the body — proving the idempotency
	// middleware, which reads the body first to hash it, restored it for BindJSON.
	first := srv.POST("/charge", map[string]any{"amount": 100}, key).AssertStatus(http.StatusOK)
	if first.Data()["amount"] != float64(100) {
		t.Fatalf("action handler did not see the body: %v", first.Data())
	}

	// Same key + same body: replay the cached response without re-running the handler.
	second := srv.POST("/charge", map[string]any{"amount": 100}, key).AssertStatus(http.StatusOK)
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("same key + same body should replay (missing Idempotent-Replayed header)")
	}
	if second.Data()["run"] != first.Data()["run"] {
		t.Errorf("handler re-ran on replay: run %v -> %v", first.Data()["run"], second.Data()["run"])
	}

	// Same key + DIFFERENT body: 422. This is the fix — it used to silently replay
	// the first response because the body hash was always the empty-body hash.
	conflict := srv.POST("/charge", map[string]any{"amount": 999}, key).AssertStatus(http.StatusUnprocessableEntity)
	if got := conflict.ErrorCode(); got != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("error code: got %q, want IDEMPOTENCY_KEY_REUSED", got)
	}

	// A different key runs the handler again (not a replay).
	other := srv.POST("/charge", map[string]any{"amount": 100},
		map[string]string{"Idempotency-Key": "charge-2"}).AssertStatus(http.StatusOK)
	if other.Header.Get("Idempotent-Replayed") == "true" {
		t.Errorf("a different key must not replay")
	}

	// Across all five requests the handler ran exactly twice: once per distinct key.
	if got := runs.Load(); got != 2 {
		t.Errorf("handler ran %d times, want 2 (one per distinct idempotency key)", got)
	}
}
