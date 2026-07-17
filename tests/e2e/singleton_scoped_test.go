package e2e

// R4 — a Singleton scoped by a forced filter is one row per tenant, resolved and
// provisioned per caller.
//
// It was one row, globally: two tenants reading GET /store_sites saw the same
// record, so the per-tenant settings/profile/storefront row — near-universal in
// B2B SaaS — did not fit the shape at all. The workaround was Headless plus a
// hand-written Action, and an Action skips Validate, so it silently lost every
// mfx tag rule and the generated schema.
//
// Pairing it with db.Tenancy did not work either: it 404'd forever (P1-9),
// because provisioning inserted a filterless row with no tenant column while the
// read applied the filter. That is why deriving the scope from the forced filters
// is safe — there was no working behaviour to break.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// StoreSite is a one-row-per-owner storefront: no id in the URL, one record per
// caller. OwnerID is unique so a concurrent first access collides rather than
// provisioning twice.
type StoreSite struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable,unique,default:"`
	Banner  string `json:"banner"   db:"banner"   mfx:"default:untitled"`
	Theme   string `json:"theme"    db:"theme"    mfx:"enum:light|dark,default:light"`
}

// scopedSingletonSrv mounts StoreSite as a singleton scoped to the X-Owner header.
func scopedSingletonSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{StoreSite{}, maniflex.ModelConfig{Singleton: true, TableName: "store_sites"}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.ForceFilter("owner_id", func(ctx *maniflex.ServerContext) any {
					if o := ctx.Request.Header.Get("X-Owner"); o != "" {
						return o
					}
					return nil
				}), maniflex.ForModel("StoreSite"))
		},
	})
}

var (
	ownerA = map[string]string{"X-Owner": "owner-a"}
	ownerB = map[string]string{"X-Owner": "owner-b"}
)

// The headline: two tenants, two rows, on the one path — and each is stamped with
// its own scope, so its author can read it back.
func TestScopedSingleton_OneRowPerTenant(t *testing.T) {
	srv := scopedSingletonSrv(t)

	a := srv.GET("/store_sites", ownerA).AssertStatus(http.StatusOK).Data()
	b := srv.GET("/store_sites", ownerB).AssertStatus(http.StatusOK).Data()

	if a["id"] == b["id"] {
		t.Fatalf("both owners resolved to id %v — the singleton is still global", a["id"])
	}
	if a["owner_id"] != "owner-a" || b["owner_id"] != "owner-b" {
		t.Errorf("owner_id: a=%v b=%v — the provisioned row was not stamped with its scope, "+
			"so its author's next read cannot see it", a["owner_id"], b["owner_id"])
	}
	// Not the global literal: one fixed id cannot name one row per scope.
	if a["id"] == maniflex.SingletonID || b["id"] == maniflex.SingletonID {
		t.Errorf("a scoped row was provisioned under SingletonID (%v / %v)", a["id"], b["id"])
	}
}

// P1-9, the bug this retires: the first access provisioned a row the read could
// not see, so every request 404'd — forever, not just once.
func TestScopedSingleton_FirstAccessDoesNot404(t *testing.T) {
	srv := scopedSingletonSrv(t)

	srv.GET("/store_sites", ownerA).AssertStatus(http.StatusOK)
	srv.GET("/store_sites", ownerA).AssertStatus(http.StatusOK)
	srv.PATCH("/store_sites", map[string]any{"banner": "x"}, ownerA).AssertStatus(http.StatusOK)
}

// Writes are the caller's own, and stay that way.
func TestScopedSingleton_WritesAreIsolated(t *testing.T) {
	srv := scopedSingletonSrv(t)

	srv.PATCH("/store_sites", map[string]any{"banner": "A's shop"}, ownerA).
		AssertStatus(http.StatusOK)
	srv.PATCH("/store_sites", map[string]any{"banner": "B's shop"}, ownerB).
		AssertStatus(http.StatusOK)

	if got := srv.GET("/store_sites", ownerA).Data()["banner"]; got != "A's shop" {
		t.Errorf("owner A reads banner %v, want %q — a write crossed tenants", got, "A's shop")
	}
	if got := srv.GET("/store_sites", ownerB).Data()["banner"]; got != "B's shop" {
		t.Errorf("owner B reads banner %v, want %q", got, "B's shop")
	}
}

// Provisioning is idempotent per scope: repeated access must resolve the same row
// rather than pile up a new one each time.
func TestScopedSingleton_ProvisioningIsIdempotent(t *testing.T) {
	srv := scopedSingletonSrv(t)

	first := srv.GET("/store_sites", ownerA).Data()["id"]
	srv.PATCH("/store_sites", map[string]any{"banner": "b"}, ownerA)
	second := srv.GET("/store_sites", ownerA).Data()["id"]
	third := srv.GET("/store_sites", ownerA).Data()["id"]

	if first != second || second != third {
		t.Errorf("ids %v, %v, %v — a fresh row was provisioned per request", first, second, third)
	}
}

// Defaults still apply to a provisioned scoped row: the point of a singleton is
// that GET answers before anything has been written.
func TestScopedSingleton_ProvisionsFromDefaults(t *testing.T) {
	srv := scopedSingletonSrv(t)

	got := srv.GET("/store_sites", ownerA).AssertStatus(http.StatusOK).Data()
	if got["banner"] != "untitled" || got["theme"] != "light" {
		t.Errorf("provisioned row = %v, want column defaults (untitled/light)", got)
	}
}

// The real argument for the feature: the Headless+Action workaround skips the
// Validate step and so loses every mfx rule. A scoped singleton must keep them.
func TestScopedSingleton_ValidationStillApplies(t *testing.T) {
	srv := scopedSingletonSrv(t)

	srv.PATCH("/store_sites", map[string]any{"theme": "chartreuse"}, ownerA).
		AssertStatus(http.StatusUnprocessableEntity)
	srv.PATCH("/store_sites", map[string]any{"theme": "dark"}, ownerA).
		AssertStatus(http.StatusOK)
	if got := srv.GET("/store_sites", ownerA).Data()["theme"]; got != "dark" {
		t.Errorf("theme = %v, want dark", got)
	}
}

// The singleton shape is unchanged: still no collection verbs, still no /{id}.
func TestScopedSingleton_ShapeUnchanged(t *testing.T) {
	srv := scopedSingletonSrv(t)

	srv.POST("/store_sites", map[string]any{"banner": "x"}, ownerA).
		AssertStatus(http.StatusMethodNotAllowed)
	srv.DELETE("/store_sites", ownerA).AssertStatus(http.StatusMethodNotAllowed)
	srv.GET("/store_sites/"+maniflex.SingletonID, ownerA).AssertStatus(http.StatusNotFound)
}

// An unscoped request on a scope-capable model still gets the global row. The
// resolver returning nil means "no scope", and a singleton with no scope is the
// one it has always been — this is what keeps every existing app unaffected.
func TestScopedSingleton_UnscopedRequestIsGlobal(t *testing.T) {
	srv := scopedSingletonSrv(t)

	got := srv.GET("/store_sites").AssertStatus(http.StatusOK).Data() // no X-Owner
	if got["id"] != maniflex.SingletonID {
		t.Errorf("id = %v, want %q — an unscoped singleton stopped being the global row",
			got["id"], maniflex.SingletonID)
	}
}

// A scope that cannot be written cannot provision a row, and must say so rather
// than create one its author's next read would not find. ForceFilterVia builds a
// nested filter: the value lives on another table, so there is nothing to stamp.
func TestScopedSingleton_UnwritableScopeFailsClosed(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{StoreSite{}, maniflex.ModelConfig{Singleton: true, TableName: "store_sites"}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
						Field:    "owner_id",
						Operator: maniflex.OpIn, // not an equality: names no single value
						Value:    "owner-a,owner-b",
						Forced:   true,
					})
					return next()
				}, maniflex.ForModel("StoreSite"))
		},
	})

	resp := srv.GET("/store_sites")
	if resp.Status < 500 {
		t.Errorf("status = %d, want 5xx — a singleton whose scope cannot be provisioned "+
			"was served instead of refused", resp.Status)
	}
}
