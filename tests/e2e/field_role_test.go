package e2e

// R9 — role-gated field writes. validate.FieldRole / validate.RestrictField are
// the write-side twin of response.RedactField. WorkflowDoc (title, status) plays
// the "owner may write title, only a superuser may write status" shape.

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/validate"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// gatedServer registers a role gate on WorkflowDoc.status and a stub Auth
// middleware that reads roles from X-Test-Roles.
func gatedServer(t *testing.T, mw maniflex.MiddlewareFunc, logger *slog.Logger) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.WorkflowDoc{}},
		Logger: logger,
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if raw := ctx.Request.Header.Get("X-Test-Roles"); raw != "" {
					ctx.Auth = &maniflex.AuthInfo{Roles: splitCSV(raw)}
				}
				return next()
			})
			s.Pipeline.Validate.Register(
				mw,
				maniflex.ForModel("WorkflowDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
			)
		},
	})
}

func roleGatedServer(t *testing.T) *testutil.Server {
	t.Helper()
	return gatedServer(t, validate.FieldRole("status", "superuser"), nil)
}

// mkDoc creates a WorkflowDoc as a superuser (who may set status) and returns id.
func mkDoc(t *testing.T, srv *testutil.Server) string {
	t.Helper()
	resp := srv.POST("/workflow_docs", map[string]any{
		"title": "doc", "status": "draft",
	}, map[string]string{"X-Test-Roles": "superuser"})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// ── the gate ────────────────────────────────────────────────────────────────

func TestFieldRole_HolderMayWriteTheField(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	resp := srv.PATCH("/workflow_docs/"+id, map[string]any{"status": "approved"},
		map[string]string{"X-Test-Roles": "superuser"})
	resp.AssertStatus(http.StatusOK)
	if got := resp.Data()["status"]; got != "approved" {
		t.Errorf("status: got %v, want approved", got)
	}
}

// The headline: a non-holder is refused loudly, not silently ignored.
func TestFieldRole_NonHolderIsRefusedNotSilentlyStripped(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	resp := srv.PATCH("/workflow_docs/"+id, map[string]any{"status": "approved"},
		map[string]string{"X-Test-Roles": "owner"})
	resp.AssertStatus(http.StatusForbidden)
	if code := resp.ErrorCode(); code != "FIELD_FORBIDDEN" {
		t.Errorf("error code: got %q, want FIELD_FORBIDDEN", code)
	}

	// And the write really did not land.
	after := srv.GET("/workflow_docs/"+id, nil)
	if got := after.Data()["status"]; got != "draft" {
		t.Errorf("status after a refused write: got %v, want draft", got)
	}
}

// A mixed write is refused whole — the gated field does not get to ride along
// with the permitted one, and the permitted one does not land either.
func TestFieldRole_MixedWriteIsRefusedWhole(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	resp := srv.PATCH("/workflow_docs/"+id,
		map[string]any{"title": "renamed", "status": "approved"},
		map[string]string{"X-Test-Roles": "owner"})
	resp.AssertStatus(http.StatusForbidden)

	after := srv.GET("/workflow_docs/"+id, nil)
	if got := after.Data()["title"]; got != "doc" {
		t.Errorf("title: got %v, want doc — a refused write must not partially apply", got)
	}
	if got := after.Data()["status"]; got != "draft" {
		t.Errorf("status: got %v, want draft", got)
	}
}

// The point of the feature: the owner keeps writing the rest of their own row.
func TestFieldRole_UngatedFieldsStayWritable(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	resp := srv.PATCH("/workflow_docs/"+id, map[string]any{"title": "renamed"},
		map[string]string{"X-Test-Roles": "owner"})
	resp.AssertStatus(http.StatusOK)
	if got := resp.Data()["title"]; got != "renamed" {
		t.Errorf("title: got %v, want renamed", got)
	}
}

// A PATCH that never mentions the field is not a write of it.
func TestFieldRole_AbsentFieldIsNotGated(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	srv.PATCH("/workflow_docs/"+id, map[string]any{"title": "x"},
		map[string]string{"X-Test-Roles": "owner"}).
		AssertStatus(http.StatusOK)
}

// Explicit null is still a write of the field.
func TestFieldRole_ExplicitNullIsStillAWrite(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	srv.PATCH("/workflow_docs/"+id, map[string]any{"status": nil},
		map[string]string{"X-Test-Roles": "owner"}).
		AssertStatus(http.StatusForbidden)
}

func TestFieldRole_GatesCreateNotOnlyUpdate(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)

	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Test-Roles": "owner"}).
		AssertStatus(http.StatusForbidden)
}

func TestFieldRole_UnauthenticatedIsRefused(t *testing.T) {
	t.Parallel()
	srv := roleGatedServer(t)
	id := mkDoc(t, srv)

	srv.PATCH("/workflow_docs/"+id, map[string]any{"status": "approved"}, nil).
		AssertStatus(http.StatusForbidden)
}

// Fail closed: an empty role list gates everything rather than nothing, matching
// auth.RequireRole and workflow.RequireRole.
func TestFieldRole_NoRolesFailsClosed(t *testing.T) {
	t.Parallel()
	srv := gatedServer(t, validate.FieldRole("status"), nil)

	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Test-Roles": "superuser"}).
		AssertStatus(http.StatusForbidden)
}

// ── RestrictField: the predicate form ───────────────────────────────────────

// The reason the predicate form exists: gates roles cannot express.
func TestRestrictField_PredicateCanGateOnAnything(t *testing.T) {
	t.Parallel()
	srv := gatedServer(t, validate.RestrictField("status",
		func(ctx *maniflex.ServerContext) bool {
			// Not a role: an arbitrary request-scoped condition.
			return ctx.Request.Header.Get("X-Billing-Admin") == "yes"
		}), nil)

	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Test-Roles": "superuser"}).
		AssertStatus(http.StatusForbidden)

	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Billing-Admin": "yes"}).
		AssertStatus(http.StatusCreated)
}

// ── the typo footgun ────────────────────────────────────────────────────────

// A gate naming a field the model does not have is inert. That is fine across
// models, and a silent hole when it is a typo — so it warns.
func TestRestrictField_UnknownFieldWarns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	srv := gatedServer(t, validate.FieldRole("staus", "superuser"), logger) // typo

	// The real field is ungated, because the gate watches a name nothing sends.
	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Test-Roles": "owner"}).
		AssertStatus(http.StatusCreated)

	if !strings.Contains(buf.String(), "staus") {
		t.Errorf("a gate on a field the model does not have must warn (it can never fire); log was:\n%s", buf.String())
	}
}

// The warning is once per model, not once per request.
func TestRestrictField_UnknownFieldWarnsOnce(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	srv := gatedServer(t, validate.FieldRole("staus", "superuser"), logger)

	for range 3 {
		srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
			map[string]string{"X-Test-Roles": "owner"}).
			AssertStatus(http.StatusCreated)
	}

	if n := strings.Count(buf.String(), "staus"); n != 1 {
		t.Errorf("warned %d times over 3 requests, want exactly 1 — a per-request warning would flood the log", n)
	}
}

// A gate on a real field must not warn.
func TestRestrictField_KnownFieldDoesNotWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	srv := gatedServer(t, validate.FieldRole("status", "superuser"), logger)
	srv.POST("/workflow_docs", map[string]any{"title": "d", "status": "draft"},
		map[string]string{"X-Test-Roles": "superuser"}).
		AssertStatus(http.StatusCreated)

	if strings.Contains(buf.String(), "does not have") {
		t.Errorf("a gate on a real field must not warn; log was:\n%s", buf.String())
	}
}
