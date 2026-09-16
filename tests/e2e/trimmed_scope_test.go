package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit PIPE-8 — ProvidesScope() must not switch a middleware off.
//
// compose drops every ProvidesScope registration from its own step
// unconditionally, on the understanding that Pipeline.scopeChain runs it
// instead. Only the full pipeline used to keep that understanding: the trimmed
// pipelines (actions, search, presign upload) built Auth → handler → Response
// and never called scopeChain. So a middleware registered on Auth — a step none
// of those operations skips — ran on an action until you added ProvidesScope(),
// the option middleware-catalogue/db.md tells you to add to every scoper, at
// which point it silently stopped. That contradicted ProvidesScope's own rule:
// "An operation that skips the step the middleware is registered on still skips
// it when hoisted."
//
//	go test ./tests/e2e/... -run TestTrimmedScope

type TrimmedScopeItem struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name"`
}

// scopeRuns counts what actually ran, per registration shape.
type scopeRuns struct {
	authHoisted int
	authPlain   int
	dbHoisted   int
}

func countRun(n *int) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		*n++
		return next()
	}
}

// trimmedScopeSrv registers the same counting middleware three ways: on Auth
// hoisted, on Auth plain, and on the DB step hoisted. Auth is a step no trimmed
// operation skips; the DB step is one they all do.
func trimmedScopeSrv(t *testing.T, runs *scopeRuns) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{TrimmedScopeItem{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(countRun(&runs.authHoisted),
				maniflex.WithName("auth-hoisted"), maniflex.ProvidesScope())
			s.Pipeline.Auth.Register(countRun(&runs.authPlain),
				maniflex.WithName("auth-plain"))
			s.Pipeline.DB.Register(countRun(&runs.dbHoisted),
				maniflex.WithName("db-hoisted"), maniflex.ProvidesScope())

			s.Action(maniflex.ActionConfig{
				Method: http.MethodPost,
				Path:   "/probe/ping",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"ok": true},
					}
					return nil
				},
			})
		},
	})
}

// TestTrimmedScope_HoistedAuthMiddlewareRunsOnActions is the bug: declaring
// ProvidesScope() on a step the operation does not skip must not stop the
// middleware running.
func TestTrimmedScope_HoistedAuthMiddlewareRunsOnActions(t *testing.T) {
	var runs scopeRuns
	srv := trimmedScopeSrv(t, &runs)

	srv.POST("/probe/ping", nil).AssertStatus(http.StatusOK)

	if runs.authHoisted != 1 {
		t.Errorf("Auth middleware with ProvidesScope() ran %d times on an action, want 1 "+
			"— adding ProvidesScope() must not switch a middleware off", runs.authHoisted)
	}
	// The control: without ProvidesScope() it always ran here.
	if runs.authPlain != 1 {
		t.Errorf("plain Auth middleware ran %d times on an action, want 1", runs.authPlain)
	}
}

// TestTrimmedScope_DbStepScoperStaysSkippedOnActions is the anti-over-reach
// pair. An action skips the DB step, so a scoper registered there must stay
// skipped — hoisting changes when a middleware runs, never whether it does. A
// fix that ran every ProvidesScope registration in the trimmed pipeline would
// break this.
func TestTrimmedScope_DbStepScoperStaysSkippedOnActions(t *testing.T) {
	var runs scopeRuns
	srv := trimmedScopeSrv(t, &runs)

	srv.POST("/probe/ping", nil).AssertStatus(http.StatusOK)

	if runs.dbHoisted != 0 {
		t.Errorf("DB-step scoper ran %d times on an action, want 0 — an action skips "+
			"the DB step, so it skips that step's scope middleware too", runs.dbHoisted)
	}
}

// TestTrimmedScope_HoistedMiddlewareStillRunsExactlyOnce is the other
// anti-over-reach pair, on the full pipeline. compose skips a ProvidesScope
// middleware in its own step precisely because scopeChain runs it; if the
// trimmed-pipeline fix had instead made compose keep it, a CRUD request would
// run it twice.
func TestTrimmedScope_HoistedMiddlewareStillRunsExactlyOnce(t *testing.T) {
	var runs scopeRuns
	srv := trimmedScopeSrv(t, &runs)

	srv.GET("/trimmed_scope_items").AssertStatus(http.StatusOK)

	if runs.authHoisted != 1 {
		t.Errorf("hoisted Auth middleware ran %d times on a CRUD list, want exactly 1",
			runs.authHoisted)
	}
	if runs.dbHoisted != 1 {
		t.Errorf("hoisted DB middleware ran %d times on a CRUD list, want exactly 1",
			runs.dbHoisted)
	}
}
