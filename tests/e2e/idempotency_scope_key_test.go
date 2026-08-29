package e2e

// The third copy of the "authenticated user id, else RemoteAddr" default key.
// db.RateLimit's was fixed in v0.10.0; db.RateLimitAction's and this one were
// not. Here the consequence is worse than a rate limit that does not apply: the
// scope is part of the cache key, so an unauthenticated retry arriving on a new
// connection does not find the first response and the operation runs a second
// time — and a retry after a network failure is exactly when a new connection
// gets used, which is the case idempotency exists for.
//
//	go test ./tests/e2e/ -run TestIdempotencyDefaultScope

import (
	"bytes"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/idempotency"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func postChargeOnFreshConnection(t *testing.T, s *testutil.Server, key string) int {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer c.CloseIdleConnections()

	req, err := http.NewRequest("POST", s.APIPath("/charge-once"),
		bytes.NewReader([]byte(`{"amount":100}`)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestIdempotencyDefaultScope_HoldsAcrossConnections(t *testing.T) {
	t.Parallel()

	store := maniflex.NewMemoryCache()
	var runs atomic.Int64

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:      "POST",
				Path:        "/charge-once",
				AllowPublic: true,
				Middleware: []maniflex.MiddlewareFunc{
					idempotency.Middleware(idempotency.Config{Store: store, TTL: time.Hour}),
				},
				Handler: func(ctx *maniflex.ServerContext) error {
					n := runs.Add(1)
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"run": n},
					}
					return nil
				},
			})
		},
	})

	postChargeOnFreshConnection(t, srv, "charge-1")
	postChargeOnFreshConnection(t, srv, "charge-1")

	if got := runs.Load(); got != 1 {
		t.Errorf("handler ran %d times for one idempotency key, want 1 — the retry arrived on a new "+
			"connection, so the ephemeral port put it in a different scope and the cached response was missed", got)
	}
}
