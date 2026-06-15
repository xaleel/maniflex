package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// runMW drives the workflow middleware with a fixture context. The test
// supplies a body, optionally a prior "current" record (for Update), and the
// expected outcome (next called vs. response set).
func runMW(t *testing.T, m *Machine, op maniflex.Operation, body map[string]any, roles []string) (resp *maniflex.APIResponse, nextCalled bool) {
	t.Helper()
	ctx := &maniflex.ServerContext{
		Operation: op,
		Model:     &maniflex.ModelMeta{Name: "Stub"},
	}
	if body != nil {
		ctx.ParsedBody = maniflex.NewRequestBody(body)
	}
	if len(roles) > 0 {
		ctx.Auth = &maniflex.AuthInfo{Roles: roles}
	}
	err := m.Middleware()(ctx, func() error {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	return ctx.Response, nextCalled
}

func TestMachine_Match_ExactPair(t *testing.T) {
	m := New("status",
		Allow("draft", "submitted"),
	)
	ok, _ := m.match("draft", "submitted")
	if !ok {
		t.Error("draft → submitted should match")
	}
	ok, _ = m.match("draft", "approved")
	if ok {
		t.Error("draft → approved should not match (no rule)")
	}
}

func TestMachine_Match_Wildcards(t *testing.T) {
	m := New("status",
		Allow("*", "archived"),  // any → archived
		Allow("draft", "*"),     // draft → any
	)
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"draft", "anywhere", true},
		{"approved", "archived", true},
		{"approved", "draft", false},
	} {
		got, _ := m.match(c.from, c.to)
		if got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestMachine_AllowAny_IsCatchAll(t *testing.T) {
	m := New("status", AllowAny())
	if ok, _ := m.match("x", "y"); !ok {
		t.Error("AllowAny should match every pair")
	}
}

func TestCreate_AllowInitial_PassFail(t *testing.T) {
	m := New("status", AllowInitial("draft", "submitted"))

	_, called := runMW(t, m, maniflex.OpCreate, map[string]any{"status": "draft"}, nil)
	if !called {
		t.Error("draft should pass AllowInitial")
	}

	resp, called2 := runMW(t, m, maniflex.OpCreate, map[string]any{"status": "paid"}, nil)
	if called2 {
		t.Error("paid should NOT pass AllowInitial")
	}
	if resp == nil || resp.Error == nil || resp.Error.Code != "INVALID_TRANSITION" {
		t.Errorf("expected 422 INVALID_TRANSITION, got %+v", resp)
	}
}

func TestCreate_WithoutAllowInitial_AlwaysPasses(t *testing.T) {
	m := New("status", Allow("draft", "submitted"))
	_, called := runMW(t, m, maniflex.OpCreate, map[string]any{"status": "anything"}, nil)
	if !called {
		t.Error("Create without AllowInitial should always pass")
	}
}

func TestCreate_StatusAbsentFromBody_IsNoOp(t *testing.T) {
	m := New("status", AllowInitial("draft"))
	_, called := runMW(t, m, maniflex.OpCreate, map[string]any{"name": "x"}, nil)
	if !called {
		t.Error("absent status should be no-op (let `required` mfx tag handle presence)")
	}
}

func TestRequireRole_SingleRolePass(t *testing.T) {
	g := RequireRole("manager")
	ctx := &maniflex.ServerContext{Auth: &maniflex.AuthInfo{Roles: []string{"manager"}}}
	if err := g.Check(ctx, "draft", "submitted"); err != nil {
		t.Errorf("RequireRole(manager) with manager role should pass, got %v", err)
	}
}

func TestRequireRole_SingleRoleFail(t *testing.T) {
	g := RequireRole("manager")
	ctx := &maniflex.ServerContext{Auth: &maniflex.AuthInfo{Roles: []string{"viewer"}}}
	err := g.Check(ctx, "draft", "submitted")
	if err == nil {
		t.Fatal("RequireRole should reject viewer")
	}
	if !strings.Contains(err.Error(), "manager") {
		t.Errorf("error should mention required role: %v", err)
	}
}

func TestRequireRole_OrSemantics(t *testing.T) {
	g := RequireRole("manager", "finance")
	ctx := &maniflex.ServerContext{Auth: &maniflex.AuthInfo{Roles: []string{"finance"}}}
	if err := g.Check(ctx, "approved", "paid"); err != nil {
		t.Errorf("RequireRole(manager, finance) should accept either: %v", err)
	}
}

func TestRequireRole_AnonymousCallerRejected(t *testing.T) {
	g := RequireRole("admin")
	ctx := &maniflex.ServerContext{} // no Auth
	if err := g.Check(ctx, "x", "y"); err == nil {
		t.Error("anonymous caller should be rejected")
	}
}

func TestGuardFunc_CustomError(t *testing.T) {
	g := GuardFunc(func(_ *maniflex.ServerContext, _, _ string) error {
		return errors.New("outside business hours")
	})
	if err := g.Check(&maniflex.ServerContext{}, "x", "y"); err == nil ||
		err.Error() != "outside business hours" {
		t.Errorf("custom guard error should surface verbatim, got %v", err)
	}
}

func TestRuleOrderingMatters_FirstMatchWins(t *testing.T) {
	g := GuardFunc(func(_ *maniflex.ServerContext, _, _ string) error {
		return errors.New("never reached")
	})
	m := New("status",
		Allow("*", "*"),    // permissive catch-all first
		Allow("a", "b", g), // narrower rule with a guard, never reached
	)
	ok, guards := m.match("a", "b")
	if !ok {
		t.Fatal("expected match")
	}
	if len(guards) != 0 {
		t.Errorf("first-match-wins broken: got %d guards from narrow rule, want 0", len(guards))
	}
}
