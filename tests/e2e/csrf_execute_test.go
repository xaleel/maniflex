package e2e_test

// AUTH-9 — auth.CSRF steps aside for Server.Execute.
//
// CSRF defends a browser against being tricked into spending credentials it
// attaches by itself. An in-process call has no browser, so the check cannot
// pass and cannot mean anything: before this, every Execute write through a
// cookie-auth app answered 403 CSRF_TOKEN_MISSING, and the only way out was to
// put a matching pair of invented values into Invocation.Header on every call —
// ceremony that proves nothing, and the header-forging Execute exists to delete.
//
// The rows that must not move are the HTTP ones. ctx.InProcess() reads a field
// only Execute can set, so a client cannot reach this branch; the tests below
// assert both halves against the same server, so a change that widened the
// exemption to real requests would fail here rather than pass quietly.

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// csrfExecDoc is the model the in-process writes below are made against.
type csrfExecDoc struct {
	maniflex.BaseModel
	Title string `json:"title"`
}

const csrfExecSecret = "csrf-execute-hs256-secret-32-byte!"

func csrfExecServer(t *testing.T, reg func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models:     []any{csrfExecDoc{}},
		Middleware: reg,
	})
}

func csrfExecPrincipal() *maniflex.AuthInfo {
	return &maniflex.AuthInfo{UserID: "system", SessionID: "sess-1"}
}

// Every unsafe operation Execute can raise passes, in both modes. Create,
// update and delete are listed separately because each one arrives with its own
// synthesised method, and the unsafe-method branch is chosen by method.
func TestCSRFExecute_UnsafeOperationsPass(t *testing.T) {
	t.Parallel()

	for _, mode := range []struct {
		name string
		opts auth.CSRFOptions
	}{
		{"double submit", auth.CSRFOptions{Mode: auth.CSRFDoubleSubmit}},
		{"signed token", auth.CSRFOptions{Mode: auth.CSRFSignedToken, Secret: "signing-secret"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			srv := csrfExecServer(t, func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.CSRF(mode.opts))
			})
			mfx := srv.ManiflexServer()

			res, err := mfx.Execute(t.Context(), maniflex.Invocation{
				Model:     "csrfExecDoc",
				Operation: maniflex.OpCreate,
				Body:      map[string]any{"title": "written in process"},
				Auth:      csrfExecPrincipal(),
			})
			if err != nil {
				t.Fatalf("in-process create: %v", err)
			}
			id, _ := res.Data.(map[string]any)["id"].(string)
			if id == "" {
				t.Fatalf("create returned no id: %+v", res.Data)
			}

			if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
				Model:     "csrfExecDoc",
				Operation: maniflex.OpUpdate,
				ID:        id,
				Body:      map[string]any{"title": "patched in process"},
				Auth:      csrfExecPrincipal(),
			}); err != nil {
				t.Fatalf("in-process update: %v", err)
			}

			if _, err := mfx.Execute(t.Context(), maniflex.Invocation{
				Model:     "csrfExecDoc",
				Operation: maniflex.OpDelete,
				ID:        id,
				Auth:      csrfExecPrincipal(),
			}); err != nil {
				t.Fatalf("in-process delete: %v", err)
			}
		})
	}
}

// An Origin allowlist is the other way the check refuses: an in-process request
// carries no Origin and no Referer, so originAllowed can only answer false.
// Stepping aside at the top is what keeps a configured allowlist from being a
// second, independent block on Execute.
func TestCSRFExecute_OriginAllowlistDoesNotBlockInProcess(t *testing.T) {
	t.Parallel()
	srv := csrfExecServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
			AllowedOrigins: []string{"https://admin.example.com"},
		}))
	})

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "csrfExecDoc",
		Operation: maniflex.OpCreate,
		Body:      map[string]any{"title": "no origin header exists here"},
		Auth:      csrfExecPrincipal(),
	}); err != nil {
		t.Fatalf("in-process create under an Origin allowlist: %v", err)
	}
}

// MUST STILL WORK: the same server that lets an Execute through still refuses a
// real browser request that carries no token. This is the whole point of the
// middleware, and the pair is asserted against one server so that a fix which
// leaked the exemption onto the HTTP path could not pass.
func TestCSRFExecute_HTTPWritesAreStillRefused(t *testing.T) {
	t.Parallel()
	srv := csrfExecServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF())
	})

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "csrfExecDoc",
		Operation: maniflex.OpCreate,
		Body:      map[string]any{"title": "in process"},
		Auth:      csrfExecPrincipal(),
	}); err != nil {
		t.Fatalf("in-process create: %v", err)
	}

	srv.POST("/csrf_exec_docs", map[string]any{"title": "over http"}).
		AssertStatus(http.StatusForbidden)
	srv.PATCH("/csrf_exec_docs/"+uuidLikeForCSRFExec, map[string]any{"title": "over http"}).
		AssertStatus(http.StatusForbidden)
	srv.DELETE("/csrf_exec_docs/" + uuidLikeForCSRFExec).
		AssertStatus(http.StatusForbidden)
}

// A record does not exist at this id; the CSRF refusal happens on the Auth step,
// long before the DB step could answer 404, so the id never needs to be real.
const uuidLikeForCSRFExec = "11111111-1111-4111-8111-111111111111"

// MUST STILL WORK, and the reason this middleware uses ctx.InProcess() alone
// rather than the InProcess-and-a-principal rule the authenticators use: an
// Execute carrying no principal is still refused — by the authenticator, with
// the 401 that names the actual problem, rather than by CSRF with a 403 about a
// header no in-process caller could ever have sent.
func TestCSRFExecute_AnonymousInProcessCallIsStillRefusedByAuth(t *testing.T) {
	t.Parallel()
	srv := csrfExecServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF())
		s.Pipeline.Auth.Register(auth.JWTAuth(csrfExecSecret))
	})

	_, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "csrfExecDoc",
		Operation: maniflex.OpCreate,
		Body:      map[string]any{"title": "nobody sent this"},
	})
	if err == nil {
		t.Fatal("an in-process call with no principal must still be refused")
	}
	var exec *maniflex.ExecuteError
	if !errors.As(err, &exec) {
		t.Fatalf("want *maniflex.ExecuteError, got %T: %v", err, err)
	}
	if exec.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d %s, want 401 from the authenticator — CSRF must not "+
			"answer for a middleware that decides nothing", exec.StatusCode, exec.Code)
	}
}

// Safe operations kept working in process before this fix — they take the
// safe-method branch, which never refused anything — so the in-process half
// below has no teeth and is not meant to. It is here for the HTTP half: the
// double-submit cookie is how a browser obtains the token it will echo back, and
// an exemption written one branch too high would stop issuing it.
//
// The in-process Set-Cookie the middleware used to write is now skipped along
// with the rest. That is not asserted, because it is not observable: Execute
// returns the envelope and discards the response writer, so there is nowhere for
// a test to look.
func TestCSRFExecute_SafeOperationsStillIssueTheCookieOverHTTP(t *testing.T) {
	t.Parallel()
	srv := csrfExecServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF())
	})

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "csrfExecDoc",
		Operation: maniflex.OpList,
		Query:     url.Values{"limit": {"1"}},
		Auth:      csrfExecPrincipal(),
	}); err != nil {
		t.Fatalf("in-process list: %v", err)
	}

	// MUST STILL WORK: a real GET still gets its double-submit cookie.
	resp := srv.GET("/csrf_exec_docs").AssertStatus(http.StatusOK)
	var issued bool
	for _, c := range resp.Header.Values("Set-Cookie") {
		if cookieNameIs(c, "csrf_token") {
			issued = true
		}
	}
	if !issued {
		t.Errorf("expected a csrf_token cookie on a real GET; headers=%v", resp.Header)
	}
}
