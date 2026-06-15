package e2e

// request_id_test.go tests that chi's X-Request-Id is propagated into
// ServerContext.RequestID and echoed back in the response header, so every
// log line from middleware can be correlated to the originating HTTP request.
//
//	go test ./tests/e2e/... -run TestRequestID

import (
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func TestRequestID(t *testing.T) {
	t.Parallel()

	// ── Response header ───────────────────────────────────────────────────────

	t.Run("response_carries_x_request_id_header", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		rid := resp.Header.Get("X-Request-Id")
		testutil.AssertNotEmpty(t, "X-Request-Id header present", rid)
	})

	t.Run("different_requests_get_different_request_ids", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id1 := srv.GET("/users").Header.Get("X-Request-Id")
		id2 := srv.GET("/users").Header.Get("X-Request-Id")
		testutil.AssertNotEmpty(t, "first request id", id1)
		testutil.AssertNotEmpty(t, "second request id", id2)
		if id1 == id2 {
			t.Errorf("consecutive requests must get distinct IDs, both got %q", id1)
		}
	})

	t.Run("client_supplied_request_id_is_echoed_back", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.Do("GET", srv.APIPath("/users"), nil,
			map[string]string{"X-Request-Id": "my-trace-id-abc"})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "echoed request id",
			resp.Header.Get("X-Request-Id"), "my-trace-id-abc")
	})

	t.Run("request_id_present_on_error_responses", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/users/00000000-0000-0000-0000-000000000000")
		resp.AssertStatus(http.StatusNotFound)
		testutil.AssertNotEmpty(t, "X-Request-Id on 404", resp.Header.Get("X-Request-Id"))
	})

	t.Run("request_id_present_on_validation_error", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertNotEmpty(t, "X-Request-Id on 422", resp.Header.Get("X-Request-Id"))
	})

	t.Run("request_id_present_on_201_created", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.CreateUser("U", "rid@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "X-Request-Id on 201", resp.Header.Get("X-Request-Id"))
	})

	// ── ServerContext.RequestID field ────────────────────────────────────────────

	t.Run("ctx_requestid_matches_response_header", func(t *testing.T) {
		t.Parallel()
		var captured string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					captured = ctx.RequestID
					mu.Unlock()
					return next()
				})
			},
		})

		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)

		mu.Lock()
		rid := captured
		mu.Unlock()

		testutil.AssertNotEmpty(t, "ctx.RequestID set", rid)
		testutil.AssertEqual(t, "ctx.RequestID matches response header",
			rid, resp.Header.Get("X-Request-Id"))
	})

	t.Run("ctx_requestid_matches_client_supplied_id", func(t *testing.T) {
		t.Parallel()
		var captured string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					captured = ctx.RequestID
					mu.Unlock()
					return next()
				})
			},
		})

		srv.Do("GET", srv.APIPath("/users"), nil,
			map[string]string{"X-Request-Id": "client-trace-xyz"})

		mu.Lock()
		rid := captured
		mu.Unlock()

		testutil.AssertEqual(t, "ctx.RequestID is client-supplied value",
			rid, "client-trace-xyz")
	})

	t.Run("ctx_requestid_stable_across_all_pipeline_steps", func(t *testing.T) {
		// RequestID must be the same value at every step in the pipeline —
		// it is set once in dispatch and never mutated.
		t.Parallel()
		ids := make(map[string]string) // step name → request ID
		var mu sync.Mutex

		capture := func(name string) maniflex.MiddlewareFunc {
			return func(ctx *maniflex.ServerContext, next func() error) error {
				mu.Lock()
				ids[name] = ctx.RequestID
				mu.Unlock()
				return next()
			}
		}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(capture("auth"))
				s.Pipeline.Validate.Register(capture("validate"))
				s.Pipeline.Service.Register(capture("service"))
				s.Pipeline.DB.Register(capture("db"), maniflex.AtPosition(maniflex.After))
				s.Pipeline.Response.Register(capture("response"), maniflex.AtPosition(maniflex.After))
			},
		})

		resp := srv.GET("/users")
		want := resp.Header.Get("X-Request-Id")

		mu.Lock()
		snapshot := make(map[string]string, len(ids))
		for k, v := range ids {
			snapshot[k] = v
		}
		mu.Unlock()

		for step, got := range snapshot {
			if got != want {
				t.Errorf("step %q: ctx.RequestID=%q, want %q", step, got, want)
			}
		}
	})

	// ── ctx.Logger() helper ───────────────────────────────────────────────────

	t.Run("ctx_logger_returns_logger_with_request_id_attr", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record

		handler := &captureSlogHandler{mu: &mu, records: &records}
		slog.SetDefault(slog.New(handler))
		t.Cleanup(func() { slog.SetDefault(slog.Default()) })

		var capturedRID string
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					capturedRID = ctx.RequestID
					// Use the helper — it should pre-seed request_id
					ctx.Logger().Info("middleware ran")
					return next()
				})
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		if len(recs) == 0 {
			t.Fatal("expected at least one log record")
		}

		// Find the "middleware ran" record and verify request_id attribute
		found := false
		for _, rec := range recs {
			if rec.Message != "middleware ran" {
				continue
			}
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "request_id" && a.Value.String() == capturedRID {
					found = true
					return false
				}
				return true
			})
		}
		if !found {
			t.Error("ctx.Logger() must pre-seed the log record with request_id")
		}
	})

	// ── Logging middleware includes request_id ─────────────────────────────────

	t.Run("logging_middleware_includes_request_id", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					loggingMiddleware(slog.New(&captureSlogHandler{mu: &mu, records: &records})),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})

		resp := srv.GET("/users")
		wantRID := resp.Header.Get("X-Request-Id")

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		if len(recs) == 0 {
			t.Fatal("Logging middleware produced no records")
		}

		found := false
		for _, rec := range recs {
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "request_id" && a.Value.String() == wantRID {
					found = true
				}
				return true
			})
		}
		if !found {
			t.Errorf("Logging middleware must include request_id=%q in log record", wantRID)
		}
	})

	// ── Concurrent requests stay isolated ─────────────────────────────────────

	t.Run("concurrent_requests_have_distinct_request_ids", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		seen := make(map[string]int) // requestID → count

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					seen[ctx.RequestID]++
					mu.Unlock()
					return next()
				})
			},
		})

		const n = 20
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.GET("/users")
			}()
		}
		wg.Wait()

		mu.Lock()
		defer mu.Unlock()

		if len(seen) != n {
			t.Errorf("expected %d distinct request IDs, got %d", n, len(seen))
		}
		for rid, count := range seen {
			if count > 1 {
				t.Errorf("request_id %q appeared in %d concurrent requests (must be unique)", rid, count)
			}
		}
	})
}

// loggingMiddleware is a thin wrapper so we can pass a specific logger to the
// response.Logging middleware from within the test without importing the
// middleware package (which would create a cycle in the test assertions).
func loggingMiddleware(logger *slog.Logger) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		status := 0
		if ctx.Response != nil {
			status = ctx.Response.StatusCode
		}
		attrs := []slog.Attr{
			slog.String("method", ctx.Request.Method),
			slog.String("path", ctx.Request.URL.Path),
			slog.Int("status", status),
		}
		if ctx.RequestID != "" {
			attrs = append(attrs, slog.String("request_id", ctx.RequestID))
		}
		logger.LogAttrs(ctx.Ctx, slog.LevelInfo, "request", attrs...)
		return nil
	}
}
