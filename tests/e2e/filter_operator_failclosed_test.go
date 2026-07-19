package e2e

// A forced filter whose operator the query builder does not implement used to be
// dropped from the WHERE clause, so the scope it expressed silently did not
// exist: a cross-tenant read AND a cross-tenant write both answered 200.
//
// Operator is a bare string type and a forced filter is built in Go, never
// parsed — so `Operator: "equals"` (the constant OpEq is "eq") compiles, boots,
// and serves. The adapter now degrades such a filter to a false predicate; this
// covers the layer above it, which refuses the request and says why, rather than
// leaving a developer to wonder where their rows went.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// badOpSrv scopes Article by org_id through a filter with a misspelt operator.
func badOpSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{Article{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					org := ctx.Request.Header.Get("X-Org")
					if org == "" {
						return next()
					}
					if ctx.Query == nil {
						ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
					}
					ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
						Field:    "org_id",
						Operator: "equals", // typo: the constant OpEq is "eq"
						Value:    org,
						Forced:   true,
					})
					return next()
				}, maniflex.ForModel("Article"))
		},
	})
}

func TestBadFilterOperator_RefusesRequest(t *testing.T) {
	srv := badOpSrv(t)
	asA := map[string]string{"X-Org": "tenant-a"}

	resp := srv.GET("/articles", asA)
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — an unrenderable scope was served instead of "+
			"refused\nbody: %s", resp.Status, resp.Body)
	}
	// The message has to name the operator, or the developer is left bisecting
	// middleware to find out why their rows vanished.
	if body := string(resp.Body); !strings.Contains(body, "equals") {
		t.Errorf("error body does not name the bad operator: %s", body)
	}
}

// The writes too: the write path reads the row back through the same filters, so
// a filter that evaporates un-scopes the write as well as the read.
func TestBadFilterOperator_RefusesWrites(t *testing.T) {
	srv := badOpSrv(t)
	asB := map[string]string{"X-Org": "tenant-b"}

	// Seed with no scope header, so the row is created before the bad filter runs.
	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "owned-by-a", "body": "B", "status": "draft", "org_id": "tenant-a"}))

	srv.PATCH("/articles/"+id, map[string]any{"title": "PWNED"}, asB).
		AssertStatus(http.StatusInternalServerError)
	srv.DELETE("/articles/"+id, asB).AssertStatus(http.StatusInternalServerError)

	// The row must be untouched: a refused request that still wrote would be the
	// worst of both.
	after := srv.GET("/articles/" + id).AssertStatus(http.StatusOK)
	if got := after.Data()["title"]; got != "owned-by-a" {
		t.Errorf("title = %v, want owned-by-a — the refused PATCH still wrote", got)
	}
}

// A correctly-spelt scope must still work — the check must reject the typo and
// nothing else.
func TestGoodFilterOperator_StillServes(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Article{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.ForceFilter("org_id", func(ctx *maniflex.ServerContext) any {
					if org := ctx.Request.Header.Get("X-Org"); org != "" {
						return org
					}
					return nil
				}), maniflex.ForModel("Article"))
		},
	})
	asA := map[string]string{"X-Org": "tenant-a"}
	asOtherOrg := map[string]string{"X-Org": "tenant-b"}
	srv.MustID(srv.POST("/articles",
		map[string]any{"title": "a", "body": "B", "status": "draft"}, asA))
	// The out-of-scope row is created *as* tenant-b. It used to be created as
	// tenant-a with org_id spelled "tenant-b" in the body, which only worked
	// because ForceFilter did not stamp creates — the fixture was relying on
	// audit 13.8, the very leak that let one tenant plant rows into another's.
	srv.MustID(srv.POST("/articles",
		map[string]any{"title": "b", "body": "B", "status": "draft"}, asOtherOrg))

	if n := len(srv.GET("/articles", asA).DataList()); n != 1 {
		t.Errorf("list = %d items, want 1 — the operator check broke a valid scope", n)
	}
}

// A client cannot reach the check: ParseFilterParam rejects an unknown operator
// at parse time with a 400, and that must stay a 400 rather than becoming the
// server-side 500 this check raises.
func TestBadFilterOperator_FromClientIsStill400(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{Article{}}})

	resp := srv.GET("/articles?filter=status:equals:draft")
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a client's bad operator is their error, not the "+
			"server's\nbody: %s", resp.Status, resp.Body)
	}
}
