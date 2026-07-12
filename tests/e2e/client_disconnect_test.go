package e2e

// A client that hangs up mid-request cancels the request context, and the
// pipeline surfaces that as context.Canceled — the same error a QueryTimeout
// produces. Both were answered with `504 TIMEOUT: request exceeded the
// configured query timeout`, blaming the server (and citing a timeout that need
// not even be configured) for the client's own decision, and writing that body
// to a socket nobody was reading. A disconnect is now 499 with no body (BUG-18).
//
// The response never reaches the client, so these tests drive the handler
// directly: an HTTP client given a cancelled context returns an error and no
// response at all, which is exactly what makes the recorded status worth getting
// right — it is all the access log, the metrics, and any After middleware have.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// serveCancelled runs one request whose context is already cancelled — a client
// that went away — through the server's handler, and returns what was written.
func serveCancelled(t *testing.T, srv *testutil.Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	srv.ManiflexServer().Handler().ServeHTTP(rec, req)
	return rec
}

// The DB step is where a disconnect usually lands: the query fails with
// context.Canceled because the request context it was handed is dead.
func TestClientDisconnect_DBStepReports499(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})

	rec := serveCancelled(t, srv, http.MethodGet, "/api/users")

	if rec.Code != maniflex.StatusClientClosedRequest {
		t.Errorf("status = %d, want %d (client closed request)", rec.Code, maniflex.StatusClientClosedRequest)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("wrote a body to a disconnected client: %q", body)
	}
}

// A middleware that gives up when the context dies returns the error up the
// pipeline, landing in writePipelineError instead.
func TestClientDisconnect_PipelineErrorReports499(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if err := ctx.Ctx.Err(); err != nil {
					return fmt.Errorf("gave up on the request: %w", err)
				}
				return next()
			})
		},
	})

	rec := serveCancelled(t, srv, http.MethodGet, "/api/users")

	if rec.Code != maniflex.StatusClientClosedRequest {
		t.Errorf("status = %d, want %d (client closed request)", rec.Code, maniflex.StatusClientClosedRequest)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("wrote a body to a disconnected client: %q", body)
	}
}

// The other half of the fix: a cancellation that is *not* the client leaving is
// still the server's problem, and still answered with 504 TIMEOUT.
func TestClientDisconnect_ServerSideCancelStill504(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				return fmt.Errorf("internal deadline: %w", context.Canceled)
			})
		},
	})

	resp := srv.GET("/users") // a live client, connected throughout
	resp.AssertStatus(http.StatusGatewayTimeout)
	if code := resp.ErrorCode(); code != "TIMEOUT" {
		t.Errorf("error code: got %q, want TIMEOUT", code)
	}
}
