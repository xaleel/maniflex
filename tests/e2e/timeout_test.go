package e2e

// timeout_test.go tests Config.QueryTimeout: a configurable per-request
// deadline attached to ServerContext.Ctx before the pipeline runs, so that
// slow DB adapter calls are cancelled and the caller receives HTTP 504
// rather than waiting indefinitely.
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestQueryTimeout

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestQueryTimeout(t *testing.T) {
	t.Parallel()

	// ── Zero timeout is off by default ────────────────────────────────────────

	t.Run("zero_timeout_disabled_normal_requests_succeed", func(t *testing.T) {
		t.Parallel()
		// Zero QueryTimeout (the default) must not affect any request.
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 0,
		})
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.CreateUser("Alice", "zt@x.com", "viewer").AssertStatus(http.StatusCreated)
	})

	// ── Timeout fires when DB is slow ─────────────────────────────────────────

	t.Run("slow_db_middleware_triggers_504", func(t *testing.T) {
		t.Parallel()
		// Use a very short timeout and a Service middleware that sleeps longer
		// than the timeout, simulating a slow DB call.
		const qTimeout = 50 * time.Millisecond

		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: qTimeout,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					// Block for longer than the query timeout.
					select {
					case <-ctx.Ctx.Done():
						// Context was cancelled — propagate so the DB step
						// sees it and converts it to a 504.
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				})
			},
		})

		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusGatewayTimeout)
	})

	t.Run("timeout_response_code_is_TIMEOUT", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 50 * time.Millisecond,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					select {
					case <-ctx.Ctx.Done():
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				})
			},
		})

		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusGatewayTimeout)
		if code := resp.ErrorCode(); code != "TIMEOUT" {
			t.Errorf("error code: got %q, want TIMEOUT", code)
		}
	})

	t.Run("timeout_response_body_is_json_error_envelope", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 50 * time.Millisecond,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					select {
					case <-ctx.Ctx.Done():
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				})
			},
		})

		srv.GET("/users").AssertJSON(func(body map[string]any) {
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("response must have 'error' object, got: %v", body)
			}
			if errObj["code"] != "TIMEOUT" {
				t.Errorf("error.code: got %v, want TIMEOUT", errObj["code"])
			}
			if errObj["message"] == nil || errObj["message"] == "" {
				t.Error("error.message must be non-empty")
			}
		})
	})

	// ── Fast requests are not affected ────────────────────────────────────────

	t.Run("fast_requests_complete_normally_with_timeout_set", func(t *testing.T) {
		t.Parallel()
		// A generous timeout should never fire for normal CRUD.
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 10 * time.Second,
		})
		srv.GET("/users").AssertStatus(http.StatusOK)
		id := srv.MustID(srv.CreateUser("Bob", "fast@x.com", "viewer"))
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
		srv.PATCH("/users/"+id, map[string]any{"name": "Bobby"}).AssertStatus(http.StatusOK)
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
	})

	// ── ctx.Ctx carries the deadline into the adapter ─────────────────────────

	t.Run("ctx_has_deadline_when_timeout_configured", func(t *testing.T) {
		t.Parallel()
		var (
			mu          sync.Mutex
			hasDeadline bool
		)

		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 5 * time.Second,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					_, ok := ctx.Ctx.Deadline()
					mu.Lock()
					hasDeadline = ok
					mu.Unlock()
					return next()
				})
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		got := hasDeadline
		mu.Unlock()
		if !got {
			t.Error("ctx.Ctx must carry a deadline when QueryTimeout > 0")
		}
	})

	t.Run("ctx_has_no_deadline_when_timeout_zero", func(t *testing.T) {
		t.Parallel()
		var (
			mu          sync.Mutex
			hasDeadline bool
		)

		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 0, // disabled
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					_, ok := ctx.Ctx.Deadline()
					mu.Lock()
					hasDeadline = ok
					mu.Unlock()
					return next()
				})
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		got := hasDeadline
		mu.Unlock()
		if got {
			t.Error("ctx.Ctx must not carry a deadline when QueryTimeout is 0")
		}
	})

	t.Run("deadline_is_approximately_query_timeout_from_now", func(t *testing.T) {
		t.Parallel()
		const qTimeout = 5 * time.Second
		var (
			mu       sync.Mutex
			deadline time.Time
		)

		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: qTimeout,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					d, _ := ctx.Ctx.Deadline()
					mu.Lock()
					deadline = d
					mu.Unlock()
					return next()
				})
			},
		})

		before := time.Now()
		srv.GET("/users").AssertStatus(http.StatusOK)
		after := time.Now()

		mu.Lock()
		d := deadline
		mu.Unlock()

		// The deadline should be within [before+timeout, after+timeout+slack].
		earliest := before.Add(qTimeout)
		latest := after.Add(qTimeout + 200*time.Millisecond)
		if d.Before(earliest) || d.After(latest) {
			t.Errorf("deadline %v not in expected range [%v, %v]", d, earliest, latest)
		}
	})

	// ── Timeout applies to all operations ────────────────────────────────────

	t.Run("timeout_applies_to_create", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 50 * time.Millisecond,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					select {
					case <-ctx.Ctx.Done():
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				}, maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		r := srv.CreateUser("X", "to_create@x.com", "viewer")
		fmt.Println(string(r.Body))
		srv.CreateUser("X", "to_create@x.com", "viewer").
			AssertStatus(http.StatusGatewayTimeout)
	})

	t.Run("timeout_applies_to_update", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 50 * time.Millisecond,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					select {
					case <-ctx.Ctx.Done():
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				}, maniflex.ForOperation(maniflex.OpUpdate))
			},
		})
		// The record does not need to exist — the Service step fires before the
		// DB step and the timeout will cancel it before a NOT_FOUND check.
		srv.PATCH("/users/00000000-0000-0000-0000-000000000001",
			map[string]any{"name": "Y"}).AssertStatus(http.StatusGatewayTimeout)
	})

	t.Run("timeout_applies_to_delete", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 50 * time.Millisecond,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					select {
					case <-ctx.Ctx.Done():
						return ctx.Ctx.Err()
					case <-time.After(5 * time.Second):
						return next()
					}
				}, maniflex.ForOperation(maniflex.OpDelete))
			},
		})
		srv.DELETE("/users/00000000-0000-0000-0000-000000000001").
			AssertStatus(http.StatusGatewayTimeout)
	})

	// ── Cancel propagates from cancel func ───────────────────────────────────

	t.Run("cancel_func_released_after_fast_request", func(t *testing.T) {
		t.Parallel()
		// The cancel func must be called even when the pipeline completes
		// normally (no timeout). This is verified by the absence of goroutine
		// leaks — httptest.Server.Close() would block if cancel were never called.
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 10 * time.Second,
		})
		for range 5 {
			srv.GET("/users").AssertStatus(http.StatusOK)
		}
		// If we reach here without deadlock the cancel func is being called.
	})

	// ── Concurrent requests each get their own independent deadline ───────────

	// The property under test is that buildContext derives each request's
	// deadline from that request's own arrival time, rather than handing every
	// request one deadline created somewhere higher up.
	//
	// This deliberately does NOT assert that simultaneous requests receive
	// distinct deadlines, which is what it used to do (audit N7). That
	// assertion tests the platform clock, not the server: time.Now has a
	// granularity of roughly 0.5-15.6ms on Windows depending on the timer
	// resolution in force, and ten context.WithTimeout calls issued inside one
	// tick legitimately produce ten identical deadlines. A standalone probe
	// collided on its first attempt. The old test survived only because an HTTP
	// round trip usually spreads requests across ticks — so it passed or failed
	// on machine speed and load, and it failed twice here under a full
	// parallel package run.
	//
	// Two waves separated by a gap that dwarfs any plausible clock granularity
	// test the real property instead: whatever happens inside a wave, the
	// second wave's deadlines must have moved forward by about the gap. A
	// shared deadline cannot move, so it fails by the full margin rather than
	// by a hair, on any clock.
	t.Run("concurrent_requests_get_independent_deadlines", func(t *testing.T) {
		t.Parallel()
		const (
			perWave      = 5
			queryTimeout = 5 * time.Second
			// Far larger than the ~15.6ms worst-case tick, so the verdict never
			// rides on timer resolution.
			gap = 250 * time.Millisecond
		)

		type observation struct {
			deadline  time.Time
			remaining time.Duration
		}
		var (
			mu  sync.Mutex
			obs []observation
		)

		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: queryTimeout,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if d, ok := ctx.Ctx.Deadline(); ok {
						mu.Lock()
						obs = append(obs, observation{deadline: d, remaining: time.Until(d)})
						mu.Unlock()
					}
					return next()
				})
			},
		})

		// wave fires perWave concurrent requests and returns only the
		// observations they produced, so the two waves cannot be confused.
		wave := func(label string) []observation {
			t.Helper()
			mu.Lock()
			start := len(obs)
			mu.Unlock()

			statuses := make([]int, perWave)
			var wg sync.WaitGroup
			for i := range perWave {
				wg.Add(1)
				go func() {
					defer wg.Done()
					statuses[i] = srv.GET("/users").Status
				}()
			}
			wg.Wait()

			// A request that never reached the pipeline records no deadline,
			// which would otherwise surface as a confusing count mismatch.
			for i, st := range statuses {
				if st != http.StatusOK {
					t.Fatalf("%s: request %d returned %d, want %d", label, i, st, http.StatusOK)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			got := append([]observation(nil), obs[start:]...)
			if len(got) != perWave {
				t.Fatalf("%s: recorded %d deadlines, want %d", label, len(got), perWave)
			}
			return got
		}

		first := wave("first wave")
		time.Sleep(gap)
		second := wave("second wave")

		// A sanity bound on the size of each deadline, not the independence
		// proof — that is the wave comparison below.
		//
		// The upper bound is exact and cannot flake: the deadline is arrival
		// plus QueryTimeout, and the reading is taken no earlier than arrival.
		// The lower bound is deliberately loose. Anything tighter is a bound on
		// how long the pipeline took to reach this middleware, which is a
		// property of machine load; a floor just under QueryTimeout would be
		// the same kind of assertion this test was rewritten to remove.
		for _, o := range append(append([]observation{}, first...), second...) {
			if o.remaining > queryTimeout {
				t.Errorf("a request saw %v remaining, which exceeds QueryTimeout %v",
					o.remaining, queryTimeout)
			}
			if o.remaining < queryTimeout/2 {
				t.Errorf("a request saw only %v remaining of QueryTimeout %v — its deadline "+
					"was not created for this request", o.remaining, queryTimeout)
			}
		}

		// The independence proof. latestFirst is the most generous reading of
		// wave one and earliestSecond the least generous of wave two, so this
		// holds however the requests interleaved within each wave.
		latestFirst := first[0].deadline
		for _, o := range first[1:] {
			if o.deadline.After(latestFirst) {
				latestFirst = o.deadline
			}
		}
		earliestSecond := second[0].deadline
		for _, o := range second[1:] {
			if o.deadline.Before(earliestSecond) {
				earliestSecond = o.deadline
			}
		}
		if advance := earliestSecond.Sub(latestFirst); advance < gap/2 {
			t.Errorf("deadlines advanced by only %v across a %v pause; every request must get "+
				"a deadline measured from its own arrival, so the second wave should be roughly "+
				"%v later than the first (a shared deadline gives 0)", advance, gap, gap)
		}
	})

	// ── Timeout does not swallow non-timeout errors ───────────────────────────

	t.Run("not_found_still_returns_404_with_timeout_set", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 10 * time.Second,
		})
		srv.GET("/users/00000000-0000-0000-0000-000000000000").
			AssertStatus(http.StatusNotFound)
	})

	t.Run("validation_error_still_returns_422_with_timeout_set", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			QueryTimeout: 10 * time.Second,
		})
		srv.POST("/users", map[string]any{}).
			AssertStatus(http.StatusUnprocessableEntity)
	})
}
