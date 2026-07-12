package e2e

// HEAD is GET with the body suppressed, and OPTIONS advertises what the route
// allows. Neither used to reach the DB step at all: HEAD answered 200 for every
// URL — including a record that does not exist — OPTIONS sent no Allow header,
// and both logged "Response step reached with ctx.DBResult == nil" on every
// request, filling the logs at load-balancer probe frequency (BUG-9).

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// lockedBuffer collects log output written from the server's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── HEAD mirrors GET ─────────────────────────────────────────────────────────

func TestHead_ItemMirrorsGetStatus(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})
	id := srv.MustID(srv.CreateUser("Alice", "alice-head@test.com", "viewer"))

	existing := srv.Do(http.MethodHead, srv.APIPath("/users/"+id), nil)
	existing.AssertStatus(http.StatusOK)
	if len(existing.Body) != 0 {
		t.Errorf("HEAD returned a %d-byte body, want none", len(existing.Body))
	}

	// The whole point: a HEAD probe of a record that isn't there must not say 200.
	srv.Do(http.MethodHead, srv.APIPath("/users/does-not-exist"), nil).
		AssertStatus(http.StatusNotFound)
}

func TestHead_CollectionReturns200WithNoBody(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})
	srv.MustID(srv.CreateUser("Bob", "bob-head@test.com", "viewer"))

	resp := srv.Do(http.MethodHead, srv.APIPath("/users"), nil)
	resp.AssertStatus(http.StatusOK)
	if len(resp.Body) != 0 {
		t.Errorf("HEAD returned a %d-byte body, want none", len(resp.Body))
	}
}

// HEAD runs the read pipeline, so middleware scoped to the read operations
// applies to it. Otherwise HEAD would be an existence oracle that walks straight
// around a ForOperation(OpRead) auth guard.
func TestHead_IsSubjectToReadScopedMiddleware(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "nope")
					return nil
				},
				maniflex.ForModel("User"),
				maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
			)
		},
	})

	srv.Do(http.MethodHead, srv.APIPath("/users/any-id"), nil).
		AssertStatus(http.StatusUnauthorized)
	srv.Do(http.MethodHead, srv.APIPath("/users"), nil).
		AssertStatus(http.StatusUnauthorized)
}

// ── OPTIONS advertises the route ─────────────────────────────────────────────

func TestOptions_SendsAllowHeader(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})

	collection := srv.Do(http.MethodOptions, srv.APIPath("/users"), nil)
	collection.AssertStatus(http.StatusNoContent)
	if got := collection.Header.Get("Allow"); got != "GET, HEAD, POST, OPTIONS" {
		t.Errorf("collection Allow = %q, want %q", got, "GET, HEAD, POST, OPTIONS")
	}

	item := srv.Do(http.MethodOptions, srv.APIPath("/users/some-id"), nil)
	item.AssertStatus(http.StatusNoContent)
	if got := item.Header.Get("Allow"); got != "GET, HEAD, PATCH, DELETE, OPTIONS" {
		t.Errorf("item Allow = %q, want %q", got, "GET, HEAD, PATCH, DELETE, OPTIONS")
	}
}

// ── No log spam ──────────────────────────────────────────────────────────────

func TestHeadOptions_DoNotWarnAboutNilDBResult(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}
	srv := testutil.NewServer(t, testutil.Options{
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	id := srv.MustID(srv.CreateUser("Carol", "carol-head@test.com", "viewer"))

	srv.Do(http.MethodHead, srv.APIPath("/users"), nil)
	srv.Do(http.MethodHead, srv.APIPath("/users/"+id), nil)
	srv.Do(http.MethodOptions, srv.APIPath("/users"), nil)
	srv.Do(http.MethodOptions, srv.APIPath("/users/"+id), nil)

	if out := logs.String(); strings.Contains(out, "DBResult == nil") {
		t.Errorf("HEAD/OPTIONS logged a spurious warning:\n%s", out)
	}
}
