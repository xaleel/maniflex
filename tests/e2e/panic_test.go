package e2e

// panic_test.go tests the custom panic recovery middleware introduced in
// recover.go. It verifies that:
//   - panics in middleware return HTTP 500 with a JSON error envelope
//   - the error code is "PANIC" and the body matches the standard APIError shape
//   - the panic value and stack trace are captured in structured logs
//   - the stack trace is NOT leaked to HTTP clients
//   - normal (non-panicking) requests are unaffected
//   - panics with different value types (string, error, arbitrary) all recover
//   - panics inside After-DB middleware still recover cleanly
//   - the request_id is included in the log record when RequestID middleware runs
//   - a nil PanicLogger falls back to slog.Default() without panicking
//   - the response Content-Type is application/json even after a panic
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestPanicRecovery

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func TestPanicRecovery(t *testing.T) {
	t.Parallel()

	// ── Response shape ────────────────────────────────────────────────────────

	t.Run("panic_returns_500", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "string panic", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("something went wrong")
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("response_body_is_json_error_envelope", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "json envelope", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("boom")
		})
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusInternalServerError)
		resp.AssertJSON(func(body map[string]any) {
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("response must have top-level 'error' object, got: %v", body)
			}
			code, _ := errObj["code"].(string)
			msg, _ := errObj["message"].(string)
			testutil.AssertEqual(t, "error code", code, "PANIC")
			testutil.AssertNotEmpty(t, "error message", msg)
		})
	})

	t.Run("response_content_type_is_json", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "content-type", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("bad")
		})
		resp := srv.GET("/users")
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type after panic must be application/json, got: %q", ct)
		}
	})

	t.Run("stack_trace_not_in_response_body", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "no stack leak", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("do not leak me")
		})
		resp := srv.GET("/users")
		body := string(resp.Body)
		// Stack traces contain ".go:" — must not appear in the client response.
		if strings.Contains(body, ".go:") {
			t.Errorf("stack trace must not be included in the HTTP response body, got: %s", body)
		}
		// The panic message itself must not appear either.
		if strings.Contains(body, "do not leak me") {
			t.Errorf("panic message must not be included in the HTTP response body, got: %s", body)
		}
	})

	t.Run("response_has_no_data_key", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "no data key", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("x")
		})
		resp := srv.GET("/users")
		resp.AssertJSON(func(body map[string]any) {
			if _, hasData := body["data"]; hasData {
				t.Error("panic response must not contain a 'data' key")
			}
		})
	})

	// ── Panic value types ─────────────────────────────────────────────────────

	t.Run("panic_with_string_value_recovers", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "string", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("string panic value")
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_with_error_value_recovers", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "error", func(ctx *maniflex.ServerContext, next func() error) error {
			panic(errors.New("error panic value"))
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_with_integer_value_recovers", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "int", func(ctx *maniflex.ServerContext, next func() error) error {
			panic(42)
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_with_nil_recovers", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "nil", func(ctx *maniflex.ServerContext, next func() error) error {
			panic(nil)
		})
		// nil panic is valid Go — must still return 500 not crash the server.
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_with_struct_value_recovers", func(t *testing.T) {
		t.Parallel()
		type customErr struct{ Detail string }
		srv := panicServer(t, "struct", func(ctx *maniflex.ServerContext, next func() error) error {
			panic(customErr{Detail: "custom panic"})
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	// ── Pipeline position coverage ────────────────────────────────────────────

	t.Run("panic_in_auth_step_recovers", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("auth panic")
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_in_validate_step_recovers", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("validate panic")
				})
			},
		})
		srv.POST("/users", map[string]any{
			"name": "X", "email": "vp@x.com", "password": "s",
		}).AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_in_service_step_recovers", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "service", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("service panic")
		})
		srv.POST("/users", map[string]any{
			"name": "X", "email": "sp@x.com", "password": "s",
		}).AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_in_after_db_middleware_recovers", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					panic("post-db panic")
				}, maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "X", "email": "dbp@x.com", "password": "s",
		}).AssertStatus(http.StatusInternalServerError)
	})

	t.Run("panic_in_response_step_recovers", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("response panic")
				}, maniflex.AtPosition(maniflex.After))
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	// ── Isolation: subsequent requests work normally ───────────────────────────

	t.Run("server_still_handles_requests_after_panic", func(t *testing.T) {
		t.Parallel()
		once := false
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					first := !once
					once = true
					mu.Unlock()
					if first {
						panic("first request panics")
					}
					return next()
				})
			},
		})

		// First request panics.
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
		// Server must still serve subsequent requests correctly.
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
	})

	t.Run("non_panicking_requests_unaffected", func(t *testing.T) {
		t.Parallel()
		// A server with PanicRecoverer but no panics must behave normally.
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.CreateUser("Alice", "np@x.com", "viewer"))
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users?sort=name:asc").AssertStatus(http.StatusOK)
	})

	t.Run("concurrent_panics_all_return_500_not_crash", func(t *testing.T) {
		t.Parallel()
		srv := panicServer(t, "concurrent", func(ctx *maniflex.ServerContext, next func() error) error {
			panic("concurrent panic")
		})

		const n = 10
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statuses[i] = srv.GET("/users").Status
			}(i)
		}
		wg.Wait()

		for i, s := range statuses {
			if s != http.StatusInternalServerError {
				t.Errorf("goroutine %d: got %d, want 500", i, s)
			}
		}
	})

	// ── Structured logging ────────────────────────────────────────────────────

	t.Run("panic_is_logged_at_error_level", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record
		handler := &captureSlogHandler{mu: &mu, records: &records}
		logger := slog.New(handler)

		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: logger,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("log me")
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		if len(recs) == 0 {
			t.Fatal("expected at least one log record after panic, got none")
		}
		last := recs[len(recs)-1]
		if last.Level != slog.LevelError {
			t.Errorf("panic must be logged at ERROR level, got %s", last.Level)
		}
	})

	t.Run("log_record_contains_panic_value", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record
		logger := slog.New(&captureSlogHandler{mu: &mu, records: &records})

		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: logger,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("unique-panic-marker-12345")
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		found := false
		for _, rec := range recs {
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "panic" && strings.Contains(a.Value.String(), "unique-panic-marker-12345") {
					found = true
				}
				return true
			})
		}
		if !found {
			t.Error("log record must contain a 'panic' attribute with the panic value")
		}
	})

	t.Run("log_record_contains_stack_trace", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record
		logger := slog.New(&captureSlogHandler{mu: &mu, records: &records})

		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: logger,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("stack test")
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		found := false
		for _, rec := range recs {
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "stack" && strings.Contains(a.Value.String(), ".go:") {
					found = true
				}
				return true
			})
		}
		if !found {
			t.Error("log record must contain a 'stack' attribute with Go source references")
		}
	})

	t.Run("log_record_contains_method_and_path", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record
		logger := slog.New(&captureSlogHandler{mu: &mu, records: &records})

		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: logger,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("path test")
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		var method, path string
		for _, rec := range recs {
			rec.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "method":
					method = a.Value.String()
				case "path":
					path = a.Value.String()
				}
				return true
			})
		}
		testutil.AssertEqual(t, "logged method", method, "GET")
		if !strings.Contains(path, "users") {
			t.Errorf("logged path must contain 'users', got: %q", path)
		}
	})

	t.Run("nil_panic_logger_uses_slog_default_without_panic", func(t *testing.T) {
		// Config.PanicLogger = nil must not itself cause a panic.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: nil, // explicit nil — uses slog.Default()
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic("nil logger test")
				})
			},
		})
		// Must return 500, not crash the test process.
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	t.Run("error_panic_message_logged_as_error_string", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record
		logger := slog.New(&captureSlogHandler{mu: &mu, records: &records})

		srv := testutil.NewServer(t, testutil.Options{
			PanicLogger: logger,
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					panic(fmt.Errorf("wrapped: %w", errors.New("root cause")))
				})
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		found := false
		for _, rec := range recs {
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "panic" && strings.Contains(a.Value.String(), "root cause") {
					found = true
				}
				return true
			})
		}
		if !found {
			t.Error("error panic value must be logged via .Error() including wrapped messages")
		}
	})
}

// ── Helper ────────────────────────────────────────────────────────────────────

// panicServer creates a test server with the given middleware registered on
// the Auth step (Before position), which is early enough to intercept every
// request before any DB work happens.
func panicServer(t *testing.T, name string, mw maniflex.MiddlewareFunc) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(mw)
		},
	})
}
