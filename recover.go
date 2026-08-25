package maniflex

import (
	"context"
	"encoding/json"
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
// One panic value is deliberately not recovered: http.ErrAbortHandler, which a
// handler panics with to abandon a response on purpose (httputil.ReverseProxy
// does this when an upstream dies mid-stream). It is re-panicked so net/http can
// close the connection silently, as the stdlib contract expects.
//
// logger may be nil; when nil slog.Default() is used.
func PanicRecoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapped so the recover block can ask whether the response has
			// already begun. chi's wrapper is used rather than a local
			// struct{http.ResponseWriter} because it re-exposes Flusher,
			// Hijacker, ReaderFrom and Pusher: SSE and websocket upgrades in
			// realtime.Hub type-assert for the first two, and a wrapper that
			// silently dropped them would break streaming everywhere at once.
			ww := chiMiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is not a failure — it is how a handler
					// says "I am abandoning this response on purpose". The stdlib
					// (httputil.ReverseProxy when an upstream dies mid-stream,
					// http.MaxBytesHandler) panics with it, and net/http's
					// convention is to let it through: the server closes the
					// connection without logging. Recovering it would log a
					// phantom panic and try to write a 500 JSON body on top of a
					// response that is already half-written, so re-panic and let
					// net/http do its job.
					if rec == http.ErrAbortHandler { //nolint:errorlint // net/http compares it by identity too
						panic(rec)
					}

					// Capture the full stack immediately; the goroutine stack
					// shrinks as we unwind so we must grab it here.
					stack := debug.Stack()

					// Derive a string description of the panic value. Shared
					// with the background-goroutine recovery so the two report
					// the same panic identically.
					panicStr := panicString(rec)

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
						// Whether the response had already begun, and therefore
						// whether the client got a 500 envelope or an aborted
						// connection. Without it an operator sees a panic with
						// no matching 500 and no reason for the difference.
						slog.Bool("response_committed", ww.Status() != 0 || ww.BytesWritten() > 0),
						slog.String("stack", string(stack)),
					)

					// Once a status line or a single body byte has gone out, the
					// envelope can no longer replace the response — it can only
					// be appended to it. The client would receive the partial
					// stream with {"error":...} glued to the end, under whatever
					// status was already sent: neither the data it was reading
					// nor a readable error. This is the hazard the
					// ErrAbortHandler branch above names, arriving by the
					// ordinary route (audit M2).
					if ww.Status() != 0 || ww.BytesWritten() > 0 {
						// Abort rather than return quietly. A truncated chunked
						// response that terminates cleanly looks *complete* to
						// the client, so a half-written export or NDJSON stream
						// would be silently short — undetectable, and the worse
						// of the two failures. Panicking with ErrAbortHandler
						// makes net/http drop the connection without a second
						// log line, so the transfer visibly fails.
						panic(http.ErrAbortHandler)
					}

					// Write the JSON error response.
					// We call WriteHeader before setting Content-Type because
					// the header must be sent before the body.
					ww.Header().Set("Content-Type", "application/json")
					ww.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(ww).Encode(map[string]any{
						"error": map[string]string{
							"code":    "PANIC",
							"message": "internal server error",
						},
					})
				}
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
