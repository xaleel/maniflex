package e2e_test

import (
	"fmt"
	"net/http"
	"testing"

	"maniflex"
	"maniflex/middleware/auth"
	"maniflex/tests/e2e/testutil"
)

// AbacDoc is a simple model used throughout the ABAC tests.
type AbacDoc struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id"`
	Secret  string `json:"secret"`
}

// ownerPolicy allows access only when the caller owns the record.
var ownerPolicy auth.Policy = func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
	if ctx.Auth == nil {
		return false, nil
	}
	ownerID, _ := resource["owner_id"].(string)
	return ownerID == ctx.Auth.UserID, nil
}

func abacServer(t *testing.T, policy auth.Policy, ops ...maniflex.Operation) *testutil.Server {
	t.Helper()
	const secret = "abac-test-secret"
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AbacDoc{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(auth.JWTAuth(secret))
			opts := []maniflex.MiddlewareOption{maniflex.ForModel("AbacDoc")}
			if len(ops) > 0 {
				opts = append(opts, maniflex.ForOperation(ops...))
			}
			srv.Pipeline.DB.Register(auth.Enforce(policy), opts...)
		},
	})
	return s
}

func abacToken(t *testing.T, userID string) map[string]string {
	t.Helper()
	const secret = "abac-test-secret"
	tok := makeJWTClaims(t, secret, map[string]any{
		"sub":   userID,
		"exp":   99999999999,
		"iat":   1000000000,
		"roles": []string{"user"},
	})
	return map[string]string{"Authorization": fmt.Sprintf("Bearer %s", tok)}
}

// TestABAC_OpRead_Allowed verifies that the policy is satisfied for an owner read.
func TestABAC_OpRead_Allowed(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpRead)

	alice := abacToken(t, "alice")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "s1"}, alice))

	s.GET("/abac_docs/"+id, alice).AssertStatus(http.StatusOK)
}

// TestABAC_OpRead_Denied verifies that a non-owner gets 403 on read.
func TestABAC_OpRead_Denied(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpRead)

	alice := abacToken(t, "alice")
	bob := abacToken(t, "bob")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "s2"}, alice))

	s.GET("/abac_docs/"+id, bob).AssertStatus(http.StatusForbidden)
}

// TestABAC_OpList_Filters verifies that list results are filtered per-row.
func TestABAC_OpList_Filters(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpList)

	alice := abacToken(t, "alice")
	bob := abacToken(t, "bob")

	s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "a1"}, alice))
	s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "a2"}, alice))
	s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "bob", "secret": "b1"}, bob))

	// Alice should see only her two records.
	resp := s.GET("/abac_docs", alice)
	resp.AssertStatus(http.StatusOK)
	items := resp.DataList()
	if len(items) != 2 {
		t.Errorf("alice list: got %d items, want 2", len(items))
	}

	// Bob should see only his one record.
	resp = s.GET("/abac_docs", bob)
	resp.AssertStatus(http.StatusOK)
	items = resp.DataList()
	if len(items) != 1 {
		t.Errorf("bob list: got %d items, want 1", len(items))
	}
}

// TestABAC_OpCreate_Allowed verifies that the policy is checked against ParsedBody.
func TestABAC_OpCreate_Allowed(t *testing.T) {
	// Allow create only when the caller sets their own owner_id.
	createPolicy := auth.Policy(func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
		if ctx.Auth == nil {
			return false, nil
		}
		proposed, _ := resource["owner_id"].(string)
		return proposed == ctx.Auth.UserID, nil
	})
	s := abacServer(t, createPolicy, maniflex.OpCreate)

	alice := abacToken(t, "alice")
	s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "ok"}, alice).
		AssertStatus(http.StatusCreated)
}

// TestABAC_OpCreate_Denied verifies that create is blocked when policy denies.
func TestABAC_OpCreate_Denied(t *testing.T) {
	createPolicy := auth.Policy(func(ctx *maniflex.ServerContext, resource map[string]any) (bool, error) {
		if ctx.Auth == nil {
			return false, nil
		}
		proposed, _ := resource["owner_id"].(string)
		return proposed == ctx.Auth.UserID, nil
	})
	s := abacServer(t, createPolicy, maniflex.OpCreate)

	alice := abacToken(t, "alice")
	// Alice tries to create a record owned by bob — should be denied.
	s.POST("/abac_docs", map[string]any{"owner_id": "bob", "secret": "bad"}, alice).
		AssertStatus(http.StatusForbidden)
}

// TestABAC_OpUpdate_Denied verifies that a non-owner cannot update.
func TestABAC_OpUpdate_Denied(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpUpdate)

	alice := abacToken(t, "alice")
	bob := abacToken(t, "bob")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "orig"}, alice))

	s.PATCH("/abac_docs/"+id, map[string]any{"secret": "hacked"}, bob).
		AssertStatus(http.StatusForbidden)
}

// TestABAC_OpUpdate_Allowed verifies that the owner can update their own record.
func TestABAC_OpUpdate_Allowed(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpUpdate)

	alice := abacToken(t, "alice")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "orig"}, alice))

	s.PATCH("/abac_docs/"+id, map[string]any{"secret": "updated"}, alice).
		AssertStatus(http.StatusOK)
}

// TestABAC_OpDelete_Denied verifies that a non-owner cannot delete.
func TestABAC_OpDelete_Denied(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpDelete)

	alice := abacToken(t, "alice")
	bob := abacToken(t, "bob")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "x"}, alice))

	s.DELETE("/abac_docs/"+id, bob).AssertStatus(http.StatusForbidden)
}

// TestABAC_OpDelete_Allowed verifies that the owner can delete their own record.
func TestABAC_OpDelete_Allowed(t *testing.T) {
	s := abacServer(t, ownerPolicy, maniflex.OpDelete)

	alice := abacToken(t, "alice")
	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "alice", "secret": "x"}, alice))

	s.DELETE("/abac_docs/"+id, alice).AssertStatus(http.StatusNoContent)
}

// TestABAC_AllOf verifies that AllOf combines policies with AND semantics.
func TestABAC_AllOf(t *testing.T) {
	alwaysAllow := auth.Policy(func(_ *maniflex.ServerContext, _ map[string]any) (bool, error) { return true, nil })
	alwaysDeny := auth.Policy(func(_ *maniflex.ServerContext, _ map[string]any) (bool, error) { return false, nil })

	combined := auth.AllOf(alwaysAllow, alwaysDeny)

	s := abacServer(t, combined, maniflex.OpRead)

	const secret = "abac-test-secret"
	tok := makeJWTClaims(t, secret, map[string]any{"sub": "u1", "exp": 99999999999, "iat": 1000000000})
	hdr := map[string]string{"Authorization": "Bearer " + tok}

	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "u1"}, hdr))

	// AllOf(allow, deny) = deny
	s.GET("/abac_docs/"+id, hdr).AssertStatus(http.StatusForbidden)
}

// TestABAC_AnyOf verifies that AnyOf combines policies with OR semantics.
func TestABAC_AnyOf(t *testing.T) {
	alwaysAllow := auth.Policy(func(_ *maniflex.ServerContext, _ map[string]any) (bool, error) { return true, nil })
	alwaysDeny := auth.Policy(func(_ *maniflex.ServerContext, _ map[string]any) (bool, error) { return false, nil })

	combined := auth.AnyOf(alwaysDeny, alwaysAllow)

	s := abacServer(t, combined, maniflex.OpRead)

	const secret = "abac-test-secret"
	tok := makeJWTClaims(t, secret, map[string]any{"sub": "u1", "exp": 99999999999, "iat": 1000000000})
	hdr := map[string]string{"Authorization": "Bearer " + tok}

	id := s.MustID(s.POST("/abac_docs", map[string]any{"owner_id": "u1"}, hdr))

	// AnyOf(deny, allow) = allow
	s.GET("/abac_docs/"+id, hdr).AssertStatus(http.StatusOK)
}

// TestABAC_NotFound_StillReturns404 verifies that a missing record produces 404
// and not a 403, even when an ABAC policy is in effect.
func TestABAC_NotFound_StillReturns404(t *testing.T) {
	s := abacServer(t, ownerPolicy)

	const secret = "abac-test-secret"
	tok := makeJWTClaims(t, secret, map[string]any{"sub": "u1", "exp": 99999999999, "iat": 1000000000})
	hdr := map[string]string{"Authorization": "Bearer " + tok}

	s.GET("/abac_docs/00000000-0000-0000-0000-000000000000", hdr).
		AssertStatus(http.StatusNotFound)
}
