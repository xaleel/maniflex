package e2e

// db.RateLimit's default key was fixed in v0.10.0 to drop the ephemeral TCP
// port. RateLimitAction takes the same RateLimitConfig but resolves its default
// through its own rateLimitCaller, which was left returning RemoteAddr verbatim
// — so the fix reached one of the three copies of that key function. The third
// is idempotency; see idempotency_scope_key_test.go.
//
// Actions are where this bites hardest: an action skips the Deserialize,
// Validate, Service and DB steps, so the standard RateLimit never runs for one
// at all, and RateLimitAction is the only rate limit an action can have.
//
//	go test ./tests/e2e/ -run TestRateLimitAction

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func rateLimitedAction(perMinute int) func(*maniflex.Server) {
	return func(s *maniflex.Server) {
		s.Action(maniflex.ActionConfig{
			Method:      "POST",
			Path:        "/reset-password",
			AllowPublic: true,
			Middleware: []maniflex.MiddlewareFunc{
				dbmw.RateLimitAction(dbmw.RateLimitConfig{RequestsPerMinute: perMinute}),
			},
			Handler: func(ctx *maniflex.ServerContext) error {
				ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{"ok": true}}
				return nil
			},
		})
	}
}

// postActionOnFreshConnection sends one request over a connection of its own, so
// the source port differs every time — what a client that does not pool looks
// like, and what anyone hammering an endpoint looks like.
func postActionOnFreshConnection(t *testing.T, s *testutil.Server) int {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer c.CloseIdleConnections()

	req, err := http.NewRequest("POST", s.APIPath("/reset-password"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestRateLimitAction_DefaultKeyHoldsAcrossConnections(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models:     []any{testutil.User{}},
		Middleware: rateLimitedAction(3),
	})

	var statuses []int
	for range 5 {
		statuses = append(statuses, postActionOnFreshConnection(t, srv))
	}

	limited := 0
	for _, s := range statuses {
		if s == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("5 unauthenticated requests over 5 connections against a limit of 3 produced statuses %v "+
			"with no 429 — each connection got its own bucket, so the limit never applied", statuses)
	}
}
