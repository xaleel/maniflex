package e2e

// P0-1 — a forced filter (db.Tenancy, db.ForceFilter) constrained reads but not
// writes: the adapter's Update/Delete carry no filter, so the emitted SQL was
// `WHERE id = ?` and any caller could write a row their own reads 404 on.
//
// tenancy_write_scope_test.go covers db.Tenancy. These cover what it does not:
// db.ForceFilter (whose only e2e coverage was OpList), that an in-scope write
// still works, that a client's own ?filter= still does not touch a write, and
// that a request with nothing forced pays for nothing.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// forceFilterSrv scopes Article to the caller's X-Org via db.ForceFilter — the
// same shape as Tenancy but without the tenant-column injection, so a leaked
// write shows up as a plain unscoped write rather than a row takeover.
func forceFilterSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
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
}

func TestForceFilter_ScopesUpdate(t *testing.T) {
	srv := forceFilterSrv(t)
	asA := map[string]string{"X-Org": "tenant-a"}
	asB := map[string]string{"X-Org": "tenant-b"}

	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "owned-by-a", "body": "B", "status": "draft", "org_id": "tenant-a"}, asA))

	srv.PATCH("/articles/"+id, map[string]any{"title": "PWNED"}, asB).
		AssertStatus(http.StatusNotFound)

	// The row must be untouched, not merely the response unhelpful.
	after := srv.GET("/articles/"+id, asA)
	after.AssertStatus(http.StatusOK)
	if got := after.Data()["title"]; got != "owned-by-a" {
		t.Errorf("title = %v, want %q — the cross-scope PATCH was refused but still wrote",
			got, "owned-by-a")
	}
}

func TestForceFilter_ScopesDelete(t *testing.T) {
	srv := forceFilterSrv(t)
	asA := map[string]string{"X-Org": "tenant-a"}
	asB := map[string]string{"X-Org": "tenant-b"}

	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "owned-by-a", "body": "B", "status": "draft", "org_id": "tenant-a"}, asA))

	srv.DELETE("/articles/"+id, asB).AssertStatus(http.StatusNotFound)
	srv.GET("/articles/"+id, asA).AssertStatus(http.StatusOK) // survives
}

// The other half: scoping must not break the writes it is supposed to allow.
func TestForceFilter_InScopeWritesStillWork(t *testing.T) {
	srv := forceFilterSrv(t)
	asA := map[string]string{"X-Org": "tenant-a"}

	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "mine", "body": "B", "status": "draft", "org_id": "tenant-a"}, asA))

	srv.PATCH("/articles/"+id, map[string]any{"title": "renamed"}, asA).
		AssertStatus(http.StatusOK)
	got := srv.GET("/articles/"+id, asA)
	if v := got.Data()["title"]; v != "renamed" {
		t.Errorf("title = %v, want %q — an in-scope update was refused or dropped", v, "renamed")
	}
	srv.DELETE("/articles/"+id, asA).AssertStatus(http.StatusNoContent)
	srv.GET("/articles/"+id, asA).AssertStatus(http.StatusNotFound)
}

// A missing record must still 404 the same way whether or not a scope applies —
// the scope check must not turn "absent" into something else.
func TestForceFilter_MissingRecordStill404(t *testing.T) {
	srv := forceFilterSrv(t)
	asA := map[string]string{"X-Org": "tenant-a"}
	missing := "00000000-0000-0000-0000-000000000000"

	srv.PATCH("/articles/"+missing, map[string]any{"title": "x"}, asA).
		AssertStatus(http.StatusNotFound)
	srv.DELETE("/articles/"+missing, asA).AssertStatus(http.StatusNotFound)
}

// A client's own ?filter= reaches Query.Filters on a PATCH as well as a GET, but
// it has never constrained a write and must not start to: a stray query
// parameter turning a PATCH into a 404 would be a surprise, and a client cannot
// be allowed to steer the check that guards them either.
func TestClientFilter_DoesNotScopeWrites(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{Article{}}})

	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "t", "body": "B", "status": "draft"}))

	// A filter that plainly does not match the row.
	srv.PATCH("/articles/"+id+"?filter=status:eq:published",
		map[string]any{"title": "renamed"}).AssertStatus(http.StatusOK)

	got := srv.GET("/articles/" + id)
	if v := got.Data()["title"]; v != "renamed" {
		t.Errorf("title = %v, want %q — a client ?filter= on a PATCH started scoping the "+
			"write, which it never did", v, "renamed")
	}

	srv.DELETE("/articles/" + id + "?filter=status:eq:published").
		AssertStatus(http.StatusNoContent)
}

// countingAdapter counts FindByID calls so the scope check's cost is measurable
// rather than asserted.
type scopeCountingAdapter struct {
	maniflex.DBAdapter
	findByID atomic.Int32
}

func (a *scopeCountingAdapter) FindByID(ctx context.Context, model *maniflex.ModelMeta,
	id string, q *maniflex.QueryParams,
) (any, error) {
	a.findByID.Add(1)
	return a.DBAdapter.FindByID(ctx, model, id, q)
}

// The scope check must cost nothing when nothing is scoped. Almost every write
// in almost every app has no forced filter, and making all of them pay a read to
// fix the ones that do would be a poor trade.
func TestWriteScope_NoForcedFilterCostsNoRead(t *testing.T) {
	var counter *scopeCountingAdapter
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Article{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			counter = &scopeCountingAdapter{DBAdapter: inner}
			return counter, nil
		},
	})

	id := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "t", "body": "B", "status": "draft"}))

	counter.findByID.Store(0)
	srv.PATCH("/articles/"+id, map[string]any{"title": "renamed"}).
		AssertStatus(http.StatusOK)
	if n := counter.findByID.Load(); n != 0 {
		t.Errorf("an unscoped PATCH issued %d FindByID read(s) — the scope check is "+
			"reading on every write, not only the ones a forced filter guards", n)
	}

	counter.findByID.Store(0)
	srv.DELETE("/articles/" + id).AssertStatus(http.StatusNoContent)
	if n := counter.findByID.Load(); n != 0 {
		t.Errorf("an unscoped DELETE issued %d FindByID read(s)", n)
	}
}
