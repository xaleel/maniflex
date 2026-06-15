package maniflex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// PanicRecoverer returns an HTTP middleware that catches panics, emits a
// structured slog log record, and writes a JSON error response consistent with
// the rest of the maniflex API — instead of the plain-text HTML page that
// chi's built-in Recoverer returns.
//
// Every panic produces:
//
//  1. A slog record at ERROR level with the fields:
//     - method, path           (request context)
//     - request_id             (from chi's RequestID middleware, when present)
//     - panic                  (the recovered value as a string)
//     - stack                  (full goroutine stack trace as a single string)
//
//  2. An HTTP 500 response with body:
//     {"error": {"code": "PANIC", "message": "internal server error"}}
//
// The stack trace is intentionally omitted from the HTTP response — it is
// available in the log and must not be leaked to API clients.
//
// logger may be nil; when nil slog.Default() is used.
func PanicRecoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// Capture the full stack immediately; the goroutine stack
					// shrinks as we unwind so we must grab it here.
					stack := debug.Stack()

					// Derive a string description of the panic value.
					var panicStr string
					switch v := rec.(type) {
					case error:
						panicStr = v.Error()
					case string:
						panicStr = v
					default:
						panicStr = fmt.Sprintf("%+v", v)
					}

					// Read the request ID set by chi's RequestID middleware.
					reqID := chiMiddleware.GetReqID(r.Context())

					// Structured log — goes to the configured slog logger.
					// The stack is attached as a single string attribute so that
					// JSON log aggregators (Datadog, CloudWatch, Loki) can index it.
					logger.LogAttrs(
						context.Background(),
						slog.LevelError,
						"panic recovered",
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("request_id", reqID),
						slog.String("panic", panicStr),
						slog.String("stack", string(stack)),
					)

					// Write the JSON error response.
					// We call WriteHeader before setting Content-Type because
					// the header must be sent before the body.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]string{
							"code":    "PANIC",
							"message": "internal server error",
						},
					})
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
