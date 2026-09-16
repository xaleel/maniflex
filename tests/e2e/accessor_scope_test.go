package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit PIPE-7 — what ModelAccessor actually scopes by.
//
// Incrementer.Increment's contract used to read "q carries the request's forced
// filters, the server-imposed scope from db.Tenancy or db.ForceFilter". It never
// did: the only caller is ModelAccessor.Increment, which passes the ActionScope,
// and on the CRUD path there is none. These two tests pin both halves of the
// corrected contract, so the text and the code cannot drift apart again.
//
//	go test ./tests/e2e/... -run TestAccessorScope

type TenantItem struct {
	maniflex.BaseModel
	OrgID   string `json:"org_id"  db:"org_id" mfx:"filterable"`
	Name    string `json:"name"    db:"name"`
	Counter int    `json:"counter" db:"counter"`
}

// seedTwoOrgs writes one row per tenant through an unscoped background context,
// so the fixture does not depend on the thing under test. It returns org-a's id
// and org-b's id.
func seedTwoOrgs(t *testing.T, srv *testutil.Server) (string, string) {
	t.Helper()
	bg := maniflex.NewBackground(t.Context(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
	a, err := bg.GetModel("TenantItem").Create(map[string]any{
		"org_id": "org-a", "name": "a", "counter": 0})
	if err != nil {
		t.Fatalf("seed org-a: %v", err)
	}
	b, err := bg.GetModel("TenantItem").Create(map[string]any{
		"org_id": "org-b", "name": "b", "counter": 0})
	if err != nil {
		t.Fatalf("seed org-b: %v", err)
	}
	return a["id"].(string), b["id"].(string)
}

// counterOf reads the counter through an unscoped background context. The
// numeric type differs by lane — SQLite hands back the struct's int, Postgres an
// int64 — so it normalises rather than asserting on one spelling.
func counterOf(t *testing.T, srv *testutil.Server, id string) int64 {
	t.Helper()
	bg := maniflex.NewBackground(t.Context(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
	row, err := bg.GetModel("TenantItem").Read(id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	switch v := row["counter"].(type) {
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		t.Fatalf("counter has unexpected type %T (%v)", v, v)
		return 0
	}
}

// TestAccessorScope_ActionScopeConstrainsIncrement is the half the contract does
// promise: under an ActionScope, an increment of a row outside it is ErrNotFound
// and writes nothing. This is what `q` carries, and what an adapter must apply.
func TestAccessorScope_ActionScopeConstrainsIncrement(t *testing.T) {
	var target string

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{TenantItem{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: http.MethodPost,
				Path:   "/probe/increment",
				Middleware: []maniflex.MiddlewareFunc{
					dbmw.TenancyAction("org_id", func(ctx *maniflex.ServerContext) string {
						return ctx.Request.Header.Get("X-Org")
					}),
				},
				Handler: func(ctx *maniflex.ServerContext) error {
					msg := ""
					if _, err := ctx.GetModel("TenantItem").Increment(target,
						map[string]any{"counter": 1}); err != nil {
						msg = err.Error()
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"err": msg},
					}
					return nil
				},
			})
		},
	})

	_, orgB := seedTwoOrgs(t, srv)
	target = orgB

	resp := srv.POST("/probe/increment", nil, map[string]string{"X-Org": "org-a"})
	resp.AssertStatus(http.StatusOK)
	resp.AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		got, _ := data["err"].(string)
		if got != maniflex.ErrNotFound.Error() {
			t.Errorf("increment across the action scope: got err %q, want %q",
				got, maniflex.ErrNotFound.Error())
		}
	})

	if c := counterOf(t, srv, orgB); c != 0 {
		t.Errorf("org-b's counter moved under another tenant's action scope: %v", c)
	}
}

// TestAccessorScope_CrudPathIsUnscoped pins the other half — the one the old
// contract denied. A db.Tenancy on the DB step scopes the request's own record
// at that step; it does not reach ctx.GetModel, so the accessor reaches any row
// by id. This is deliberate (the DB step is the enforcement point for generated
// routes, and the accessor is how middleware reaches rows the request is not
// about), and it is what the doc now says. If this test ever fails, the contract
// in increment.go and the Scoping section of model-accessor.md must change with
// it — the point is that the two cannot drift apart silently again.
func TestAccessorScope_CrudPathIsUnscoped(t *testing.T) {
	var target string

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{TenantItem{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
					return ctx.Request.Header.Get("X-Org")
				}),
				maniflex.ForModel("TenantItem"), maniflex.ProvidesScope())

			s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if _, err := ctx.GetModel("TenantItem").Increment(target,
					map[string]any{"counter": 1}); err != nil {
					t.Errorf("accessor increment on the CRUD path: unexpected error %v", err)
				}
				return next()
			}, maniflex.ForModel("TenantItem"), maniflex.ForOperation(maniflex.OpUpdate))
		},
	})

	orgA, orgB := seedTwoOrgs(t, srv)
	target = orgB

	srv.PATCH("/tenant_items/"+orgA, map[string]any{"name": "a2"},
		map[string]string{"X-Org": "org-a"}).AssertStatus(http.StatusOK)

	// The request's forced filter constrains the request's own row, not the
	// accessor: org-b's counter moved.
	if c := counterOf(t, srv, orgB); c != 1 {
		t.Errorf("expected the accessor to be unscoped on the CRUD path "+
			"(org-b counter 1); got %v — if this is now scoped, update the "+
			"Incrementer contract and model-accessor.md to match", c)
	}
}
