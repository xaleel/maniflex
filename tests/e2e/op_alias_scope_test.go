package e2e

// Audit MS-8: scoping written for the obvious operation silently missed the
// derived route. OpExport, OpReadAttachment and OpReadHistory are operation
// constants of their own, so db.Tenancy registered ForOperation(OpList) scoped
// the list and left GET /:model/export returning every tenant's rows — and
// ForOperation(OpRead) left the per-record history and attachment routes
// unscoped the same way.
//
// A registration naming a base read operation now also applies to the routes
// that are the same read in another shape. The implication is one-way.
//
//	go test ./tests/e2e/... -run TestOpAlias

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type oaDoc struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	OrgID string `json:"org_id" db:"org_id" mfx:"filterable"`
	Title string `json:"title"  db:"title"`
}

var (
	oaAsA = map[string]string{"X-Org": "tenant-a"}
	oaAsB = map[string]string{"X-Org": "tenant-b"}
)

// oaServer scopes tenancy to the BASE operations only — never naming OpExport,
// OpReadHistory or OpReadAttachment. That is exactly the registration the audit
// found leaking, and it must now cover the derived routes.
func oaServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{oaDoc{}, maniflex.ModelConfig{
			Versioned:     true,
			ExportEnabled: true,
		}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
					if o := ctx.Request.Header.Get("X-Org"); o != "" {
						return o
					}
					return "tenant-a"
				}),
				maniflex.ForModel("oaDoc"),
				maniflex.ForOperation(
					maniflex.OpList, maniflex.OpRead,
					maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
			)
		},
	})
}

// Export is a list. Tenancy scoped to OpList must cover it.
func TestOpAlias_ExportIsCoveredByListScope(t *testing.T) {
	srv := oaServer(t)
	srv.MustID(srv.POST("/oa_docs", map[string]any{"title": "owned-by-a"}, oaAsA))

	// The owner sees the row in both shapes.
	testutil.AssertLen(t, "owner list", srv.GET("/oa_docs", oaAsA).DataList(), 1)
	if !strings.Contains(string(srv.GET("/oa_docs/export?format=csv", oaAsA).Body), "owned-by-a") {
		t.Fatal("precondition: the owner's export should contain their own row")
	}

	// The other tenant sees it in neither. The list was already scoped; the
	// export is what leaked.
	testutil.AssertLen(t, "cross-tenant list", srv.GET("/oa_docs", oaAsB).DataList(), 0)

	body := string(srv.GET("/oa_docs/export?format=csv", oaAsB).Body)
	if strings.Contains(body, "owned-by-a") {
		t.Errorf("cross-tenant export leaked another tenant's row — tenancy was "+
			"registered ForOperation(OpList) and must cover the export: %q", body)
	}
}

// A per-record history read is a read. Tenancy scoped to OpRead must cover it.
// This is the instance that reopened MS-4.
func TestOpAlias_HistoryIsCoveredByReadScope(t *testing.T) {
	srv := oaServer(t)
	id := srv.MustID(srv.POST("/oa_docs", map[string]any{"title": "owned-by-a"}, oaAsA))
	srv.PATCH("/oa_docs/"+id, map[string]any{"title": "v2"}, oaAsA).
		AssertStatus(http.StatusOK)

	// The owner can read the record and its history.
	srv.GET("/oa_docs/"+id, oaAsA).AssertStatus(http.StatusOK)
	testutil.AssertLen(t, "owner history",
		srv.GET("/oa_docs/"+id+"/history", oaAsA).DataList(), 2)

	// The other tenant can read neither, and gets the same answer for both.
	srv.GET("/oa_docs/"+id, oaAsB).AssertStatus(http.StatusNotFound)
	if got := srv.GET("/oa_docs/"+id+"/history", oaAsB).Status; got != http.StatusNotFound {
		t.Errorf("cross-tenant history read: got %d, want 404 — tenancy was registered "+
			"ForOperation(OpRead) and must cover the history route", got)
	}
}

// The implication is one-way: naming a derived operation must not widen a
// middleware onto the base one. Without this the alias could be implemented as a
// symmetric equivalence and still pass everything above, while quietly running
// export-only middleware on every list request.
func TestOpAlias_ImplicationIsOneWay(t *testing.T) {
	var ranOn []string
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{oaDoc{}, maniflex.ModelConfig{ExportEnabled: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ranOn = append(ranOn, string(ctx.Operation))
					return next()
				},
				maniflex.ForModel("oaDoc"),
				maniflex.ForOperation(maniflex.OpExport),
				maniflex.WithName("export-only"),
			)
		},
	})

	srv.POST("/oa_docs", map[string]any{"title": "x"})
	srv.GET("/oa_docs")
	if len(ranOn) != 0 {
		t.Errorf("export-only middleware ran on %v — ForOperation(OpExport) must not "+
			"widen onto the base operation", ranOn)
	}

	srv.GET("/oa_docs/export?format=csv")
	if len(ranOn) != 1 || ranOn[0] != "export" {
		t.Errorf("export-only middleware should have run exactly once on the export, got %v", ranOn)
	}
}

// An unfiltered registration still applies everywhere — the alias must not turn
// "all operations" into a narrower set.
func TestOpAlias_UnfilteredStillAppliesEverywhere(t *testing.T) {
	var seen []string
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{oaDoc{}, maniflex.ModelConfig{ExportEnabled: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					seen = append(seen, string(ctx.Operation))
					return next()
				}, maniflex.ForModel("oaDoc"))
		},
	})

	srv.POST("/oa_docs", map[string]any{"title": "x"})
	srv.GET("/oa_docs")
	srv.GET("/oa_docs/export?format=csv")

	for _, want := range []string{"create", "list", "export"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("an unfiltered middleware must run for %q; it ran for %v", want, seen)
		}
	}
}
