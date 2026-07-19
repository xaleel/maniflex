package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit MS-4: the synthesized {model}_history model used to mount the full read
// surface at /{model}_history, and per-model middleware is registered
// ForModel(parent) — not ForModel(parentHistory). So an app that protected
// Invoice with ModelConfig.Middleware.Auth, or scoped it with db.Tenancy
// ForModel("Invoice"), left GET /invoice_history unauthenticated and unscoped:
// every tenant's history, readable by anyone who knew the URL. Chained with MS-3
// (snapshots held plaintext) that was cross-tenant exfiltration of encrypted and
// write-only fields.
//
// The history model is Headless now and history is reached through
// GET /:model/{id}/history, which runs the parent's read pipeline first.
//
//	go test ./tests/e2e/... -run TestHistoryScope

// HistScoped is versioned and carries a tenant column. It soft-deletes so the
// deleted-record case can be exercised too.
type HistScoped struct {
	maniflex.BaseModel `mfx:"versioned"`
	maniflex.WithDeletedAt
	OrgID string `json:"org_id" db:"org_id" mfx:"filterable"`
	Title string `json:"title"  db:"title"`
}

func histScopedServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{HistScoped{}},
		Middleware: func(s *maniflex.Server) {
			// Scoped to the parent only — exactly how an app writes it, and
			// exactly what used to leave the history table wide open.
			s.Pipeline.DB.Register(
				dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
					if org := ctx.Request.Header.Get("X-Org"); org != "" {
						return org
					}
					return "tenant-a"
				}), maniflex.ForModel("HistScoped"))
		},
	})
}

var (
	histAsA = map[string]string{"X-Org": "tenant-a"}
	histAsB = map[string]string{"X-Org": "tenant-b"}
)

// The headline finding: one tenant must not read another's history.
func TestHistoryScope_CrossTenantReadIsRefused(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	id := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "secret-a"}, histAsA))
	srv.PATCH("/hist_scopeds/"+id, map[string]any{"title": "secret-a-v2"}, histAsA).
		AssertStatus(http.StatusOK)

	// The owner reads it fine.
	testutil.AssertLen(t, "owner history",
		srv.GET("/hist_scopeds/"+id+"/history", histAsA).DataList(), 2)

	// The other tenant gets the same 404 the record itself gives them, so the
	// endpoint cannot be used to learn that the id exists.
	resp := srv.GET("/hist_scopeds/"+id+"/history", histAsB)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("cross-tenant history read: got %d, want 404 — tenant-b must not "+
			"see tenant-a's history; body: %s", resp.Status, resp.Body)
	}
	srv.GET("/hist_scopeds/"+id, histAsB).AssertStatus(http.StatusNotFound)
}

// The flat route is the one that was exposed. It must not exist at all — for
// either tenant, authenticated or not.
func TestHistoryScope_FlatHistoryRouteIsNotMounted(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "secret-a"}, histAsA))

	for _, path := range []string{
		"/hist_scoped_history",
		"/hist_scoped_history?filter=operation:eq:create",
	} {
		if got := srv.GET(path, histAsB).Status; got != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404 — the unscoped flat history route "+
				"must not be mounted", path, got)
		}
		// And unauthenticated, which is how the audit found it.
		if got := srv.GET(path).Status; got != http.StatusNotFound {
			t.Errorf("GET %s (no tenant header): got %d, want 404", path, got)
		}
	}
}

// A record that does not exist is a 404, not a 200 with an empty list — an empty
// list would confirm the id is absent, which is a different answer from "not
// yours" and leaks the difference.
func TestHistoryScope_UnknownRecordIs404(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	srv.GET("/hist_scopeds/00000000-0000-0000-0000-000000000000/history", histAsA).
		AssertStatus(http.StatusNotFound)
}

// A soft-deleted record keeps its history, and keeps its scope with it. This is
// the case the ScopeChecker exists for: the ordinary read path applies the
// soft-delete condition unconditionally, so without it the delete entry — the
// one an audit usually wants — would be unreachable for everyone.
func TestHistoryScope_SoftDeletedRecordKeepsScopedHistory(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	id := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "doomed"}, histAsA))
	srv.DELETE("/hist_scopeds/"+id, histAsA).AssertStatus(http.StatusNoContent)

	// The owner still sees the history, including the delete row.
	items := srv.GET("/hist_scopeds/"+id+"/history", histAsA).DataList()
	testutil.AssertLen(t, "history after soft delete", items, 2)
	newest := items[0].(map[string]any)
	testutil.AssertEqual(t, "newest operation", newest["operation"], "delete")

	// Soft-deleted must not mean unscoped — the whole risk of reaching past the
	// soft-delete condition is that the scope gets dropped along with it.
	if got := srv.GET("/hist_scopeds/"+id+"/history", histAsB).Status; got != http.StatusNotFound {
		t.Errorf("cross-tenant read of a soft-deleted record's history: got %d, want 404", got)
	}
}

// Newest first, so a caller reading page 1 sees the most recent change.
func TestHistoryScope_NewestFirstAndPaginated(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	id := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "v1"}, histAsA))
	for _, title := range []string{"v2", "v3"} {
		srv.PATCH("/hist_scopeds/"+id, map[string]any{"title": title}, histAsA).
			AssertStatus(http.StatusOK)
	}

	items := srv.GET("/hist_scopeds/"+id+"/history", histAsA).DataList()
	testutil.AssertLen(t, "history rows", items, 3)
	for i, want := range []float64{3, 2, 1} {
		got := items[i].(map[string]any)["version"]
		if got != want {
			t.Errorf("items[%d].version = %v, want %v (newest first)", i, got, want)
		}
	}

	page2 := srv.GET("/hist_scopeds/"+id+"/history?page=2&limit=1", histAsA).DataList()
	testutil.AssertLen(t, "page 2 at limit 1", page2, 1)
	testutil.AssertEqual(t, "page 2 version", page2[0].(map[string]any)["version"], float64(2))
}

// The spec must describe the route the router actually mounts. The history
// model is Headless, so it emits no paths of its own — without an explicit
// entry the endpoint would be undocumented, and a generated client would not
// know it exists.
func TestHistoryScope_OpenAPIDocumentsTheRoute(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	resp := srv.GET("/openapi.json").AssertStatus(http.StatusOK)
	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths object")
	}

	if _, exists := paths["/hist_scoped_history"]; exists {
		t.Error("the flat history path is documented but not mounted")
	}
	item, exists := paths["/hist_scopeds/{id}/history"]
	if !exists {
		t.Fatalf("the history route is mounted but undocumented; paths: %v", keysOf(paths))
	}
	get, ok := item.(map[string]any)["get"].(map[string]any)
	if !ok {
		t.Fatal("history path has no GET operation")
	}
	if _, has := get["responses"].(map[string]any)["404"]; !has {
		t.Error("the 404 an out-of-scope caller receives should be documented")
	}

	// The referenced schema must exist, or the spec is unusable by a generator.
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	if _, has := schemas["HistScopedHistoryListResponse"]; !has {
		t.Error("the history list response schema is referenced but not defined")
	}
}

// noScopeCheck hides ScopeChecker from an adapter that has it.
//
// Embedding the *interface* rather than the concrete adapter is what does it:
// only DBAdapter's own methods are promoted, so ExistsInScope — which is not one
// of them — does not come along. This is what a third-party adapter written
// against the documented DBAdapter interface looks like from the framework's
// side, and every bundled adapter implements ScopeChecker, so without this
// wrapper the fallback branch would never be executed by any test.
type noScopeCheck struct{ maniflex.DBAdapter }

func histNoScopeCheckServer(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{HistScoped{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(filepath.Join(t.TempDir(), "noscope.db"), reg)
			if err != nil {
				return nil, err
			}
			return noScopeCheck{inner}, nil
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
					if org := ctx.Request.Header.Get("X-Org"); org != "" {
						return org
					}
					return "tenant-a"
				}), maniflex.ForModel("HistScoped"))
		},
	})
	if _, ok := srv.ManiflexServer().DB().(maniflex.ScopeChecker); ok {
		t.Fatal("precondition broken: the adapter still implements ScopeChecker, " +
			"so this test would exercise the fast path and prove nothing")
	}
	return srv
}

// An adapter that does not implement ScopeChecker must still be secure. It gives
// up exactly one thing — a soft-deleted record's history — and gives up nothing
// about who may read a live record's.
func TestHistoryScope_AdapterWithoutScopeCheckerStillScopes(t *testing.T) {
	t.Parallel()
	srv := histNoScopeCheckServer(t)

	id := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "secret-a"}, histAsA))
	srv.PATCH("/hist_scopeds/"+id, map[string]any{"title": "v2"}, histAsA).
		AssertStatus(http.StatusOK)

	// The owner reads history normally — the fallback is a working code path,
	// not merely a safe one.
	testutil.AssertLen(t, "owner history",
		srv.GET("/hist_scopeds/"+id+"/history", histAsA).DataList(), 2)

	// And the scope still holds: this is the security property, and it must not
	// depend on an optional capability.
	if got := srv.GET("/hist_scopeds/"+id+"/history", histAsB).Status; got != http.StatusNotFound {
		t.Errorf("cross-tenant history read without ScopeChecker: got %d, want 404", got)
	}

	// The documented degradation, asserted so it stays a known trade-off rather
	// than becoming a surprise: with no ScopeChecker, the ordinary read applies
	// the soft-delete condition and the history goes with it.
	srv.DELETE("/hist_scopeds/"+id, histAsA).AssertStatus(http.StatusNoContent)
	if got := srv.GET("/hist_scopeds/"+id+"/history", histAsA).Status; got != http.StatusNotFound {
		t.Errorf("soft-deleted history without ScopeChecker: got %d, want 404 "+
			"(the documented fallback behaviour)", got)
	}
}

// A client filter must not be able to widen the read to another record. Two
// independent things stop it, and both are asserted because either alone could
// regress silently.
func TestHistoryScope_ClientFilterCannotWidenToAnotherRecord(t *testing.T) {
	t.Parallel()
	srv := histScopedServer(t)

	mine := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "mine"}, histAsA))
	other := srv.MustID(srv.POST("/hist_scopeds", map[string]any{"title": "other"}, histAsA))

	// 1. Query params are parsed against the PARENT model, so a filter naming a
	//    history column is rejected before the DB step — record_id is not a
	//    field of HistScoped.
	resp := srv.GET("/hist_scopeds/"+mine+"/history?filter=record_id:eq:"+other, histAsA)
	resp.AssertStatus(http.StatusBadRequest)

	// 2. And a filter that IS a valid parent field is simply not carried into
	//    the history query, which is scoped by the Forced record_id alone.
	items := srv.GET("/hist_scopeds/"+mine+"/history?filter=org_id:eq:tenant-b", histAsA).DataList()
	testutil.AssertLen(t, "history rows", items, 1)
	testutil.AssertEqual(t, "record_id",
		items[0].(map[string]any)["record_id"], mine)
}
