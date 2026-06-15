package e2e

// 5.5 — middleware/workflow state machine.

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/middleware/workflow"
	"maniflex/tests/e2e/testutil"
)

// workflowServer wires WorkflowDoc with a status machine and a stub Auth
// middleware that pulls roles from an X-Test-Roles header so e2e tests can
// drive guard checks.
func workflowServer(t *testing.T) *testutil.Server {
	t.Helper()
	sm := workflow.New("status",
		workflow.Allow("draft", "submitted"),
		workflow.Allow("submitted", "approved", workflow.RequireRole("manager")),
		workflow.Allow("submitted", "rejected", workflow.RequireRole("manager")),
		workflow.Allow("approved", "paid", workflow.RequireRole("finance")),
		workflow.AllowAny(workflow.RequireRole("admin")),
		workflow.AllowInitial("draft", "submitted"),
	)

	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.WorkflowDoc{}},
		Middleware: func(s *maniflex.Server) {
			// Roles middleware: trust X-Test-Roles header.
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if raw := ctx.Request.Header.Get("X-Test-Roles"); raw != "" {
					ctx.Auth = &maniflex.AuthInfo{Roles: splitCSV(raw)}
				}
				return next()
			})
			s.Pipeline.Validate.Register(
				sm.Middleware(),
				maniflex.ForModel("WorkflowDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
			)
		},
	})
}

func splitCSV(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// post creates a workflow doc with the given status and returns its ID.
func post(t *testing.T, srv *testutil.Server, status string, roles string) string {
	t.Helper()
	headers := map[string]string{}
	if roles != "" {
		headers["X-Test-Roles"] = roles
	}
	resp := srv.POST("/workflow_docs", map[string]any{
		"title":  "wf",
		"status": status,
	}, headers)
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// patch issues a PATCH with optional roles.
func patch(srv *testutil.Server, id, status, roles string) *testutil.Response {
	headers := map[string]string{}
	if roles != "" {
		headers["X-Test-Roles"] = roles
	}
	body := map[string]any{}
	if status != "" {
		body["status"] = status
	}
	return srv.PATCH("/workflow_docs/"+id, body, headers)
}

func TestWorkflow_CreateWithAllowedInitial(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	post(t, srv, "draft", "")
	post(t, srv, "submitted", "")
}

func TestWorkflow_CreateWithDisallowedInitial(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	resp := srv.POST("/workflow_docs", map[string]any{
		"title":  "wf",
		"status": "paid",
	}, nil)
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "INVALID_TRANSITION" {
		t.Errorf("error code: got %q, want INVALID_TRANSITION", code)
	}
}

func TestWorkflow_DraftToSubmittedNoRoleNeeded(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "draft", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)
}

func TestWorkflow_SubmittedToApprovedWithoutRoleRejected(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "submitted", "")
	resp := patch(srv, id, "approved", "")
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "INVALID_TRANSITION" {
		t.Errorf("error code: got %q, want INVALID_TRANSITION", code)
	}
}

func TestWorkflow_SubmittedToApprovedWithRoleAllowed(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "submitted", "")
	patch(srv, id, "approved", "manager").AssertStatus(http.StatusOK)
}

func TestWorkflow_AdminEscapeHatch(t *testing.T) {
	// Admin can jump any pair, including ones not whitelisted.
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "draft", "")
	// draft → paid is not in the rule table; only AllowAny(admin) matches.
	patch(srv, id, "paid", "admin").AssertStatus(http.StatusOK)
}

func TestWorkflow_NoStatusFieldInBodyIsNoOp(t *testing.T) {
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "submitted", "")
	// PATCH without a status field — workflow middleware must not run.
	resp := srv.PATCH("/workflow_docs/"+id, map[string]any{
		"title": "renamed",
	}, nil)
	resp.AssertStatus(http.StatusOK)
}

func TestWorkflow_SameStateIsNoOp(t *testing.T) {
	// PATCH that sets status to its current value — no transition attempted.
	t.Parallel()
	srv := workflowServer(t)
	id := post(t, srv, "submitted", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)
}

func TestWorkflow_NotFoundProducesStandard404(t *testing.T) {
	// A PATCH against a non-existent record should produce 404, not
	// INVALID_TRANSITION — the workflow check defers to the DB step.
	t.Parallel()
	srv := workflowServer(t)
	resp := patch(srv, "does-not-exist", "submitted", "")
	resp.AssertStatus(http.StatusNotFound)
}
