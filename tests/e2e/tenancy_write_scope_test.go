package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestTenancy_WriteScoping probes whether db.Tenancy constrains OpUpdate and
// OpDelete on generated CRUD routes.
//
// The shipped documentation (docs/src/middleware-catalogue/db.md) states that
// ForceFilter "injects a filter on every list, read, update, and delete", and
// db.Tenancy's own comment (middleware/db/query.go) claims "the filter we add
// below is what keeps cross-tenant writes honest".
//
// These tests record the ACTUAL behaviour for tenant-b acting on a row owned by
// tenant-a. They are written to document reality, not to assert the doc.
func TestTenancy_WriteScoping(t *testing.T) {
	newSrv := func(t *testing.T) *testutil.Server {
		return testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
						if org := ctx.Request.Header.Get("X-Org"); org != "" {
							return org
						}
						return "tenant-a"
					}), maniflex.ForModel("Article"))
			},
		})
	}

	asA := map[string]string{"X-Org": "tenant-a"}
	asB := map[string]string{"X-Org": "tenant-b"}

	t.Run("cross_tenant_read_is_scoped", func(t *testing.T) {
		srv := newSrv(t)
		id := srv.MustID(srv.POST("/articles",
			map[string]any{"title": "A", "body": "B", "status": "draft"}, asA))

		got := srv.GET("/articles/"+id, asB)
		t.Logf("cross-tenant GET  /articles/{id} -> %d", got.Status)
		got.AssertStatus(http.StatusNotFound) // reads ARE scoped
	})

	t.Run("cross_tenant_update", func(t *testing.T) {
		srv := newSrv(t)
		id := srv.MustID(srv.POST("/articles",
			map[string]any{"title": "owned-by-a", "body": "B", "status": "draft"}, asA))

		resp := srv.PATCH("/articles/"+id, map[string]any{"title": "PWNED"}, asB)
		t.Logf("cross-tenant PATCH /articles/{id} -> %d", resp.Status)

		// Did tenant-b's write land on tenant-a's row?
		after := srv.GET("/articles/"+id, asA)
		t.Logf("  owner (tenant-a) re-read -> %d", after.Status)

		// Tenancy also SetFields the tenant column on OpUpdate, so the row is
		// not merely written — its ownership is reassigned to the attacker.
		stolen := srv.GET("/articles/"+id, asB)
		t.Logf("  attacker (tenant-b) re-read -> %d", stolen.Status)
		if stolen.Status == http.StatusOK {
			t.Logf("  attacker now sees title=%q org_id=%v",
				stolen.Data()["title"], stolen.Data()["org_id"])
		}

		if resp.Status == http.StatusOK {
			t.Errorf("SECURITY: cross-tenant PATCH succeeded (200); expected 404")
		}
		if after.Status == http.StatusNotFound && stolen.Status == http.StatusOK {
			t.Errorf("SECURITY: row ownership transferred to attacker — " +
				"owner now 404s, attacker reads it")
		}
	})

	t.Run("cross_tenant_delete", func(t *testing.T) {
		srv := newSrv(t)
		id := srv.MustID(srv.POST("/articles",
			map[string]any{"title": "owned-by-a", "body": "B", "status": "draft"}, asA))

		resp := srv.DELETE("/articles/"+id, asB)
		t.Logf("cross-tenant DELETE /articles/{id} -> %d", resp.Status)

		after := srv.GET("/articles/"+id, asA)
		t.Logf("  owner (tenant-a) re-read after delete -> %d", after.Status)

		if resp.Status == http.StatusOK || resp.Status == http.StatusNoContent {
			t.Errorf("SECURITY: cross-tenant DELETE succeeded (%d); expected 404", resp.Status)
		}
	})
}
