package e2e_test

// 13.12 — a scoped Singleton's row must be resolvable before Validate.
//
// Scope is conventionally registered on the DB step, which runs after Validate.
// So at validation time ctx.ResourceID still held the SingletonID placeholder,
// and validate.UniqueField's "exclude the record under edit" clause rendered as
// `id != 'singleton'` — which excludes nothing. The row matched its own value and
// PATCH answered 422 "already taken" against itself.
//
// maniflex.ProvidesScope() hoists the scope middleware to run right after
// Deserialize, so ctx.ResolveResourceID() can answer.

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/validate"
)

// HoistSite is the scoped-singleton shape with a field guarded by
// validate.UniqueField — one storefront per owner, slugs unique across owners.
type HoistSite struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable,default:"`
	Slug    string `json:"slug"     db:"slug"     mfx:"default:x"`
}

// hoistSrv mounts HoistSite as a singleton scoped to X-Owner. When scopeHoisted
// is set the scope middleware declares ProvidesScope(); otherwise it is
// registered the conventional way, which is the broken arrangement.
func hoistSrv(t *testing.T, scopeHoisted bool) string {
	t.Helper()

	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(HoistSite{}, maniflex.ModelConfig{Singleton: true, TableName: "hoist_sites"})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	scope := dbmw.ForceFilter("owner_id", func(ctx *maniflex.ServerContext) any {
		if o := ctx.Request.Header.Get("X-Owner"); o != "" {
			return o
		}
		return nil
	})
	opts := []maniflex.MiddlewareOption{maniflex.ForModel("HoistSite")}
	if scopeHoisted {
		opts = append(opts, maniflex.ProvidesScope())
	}
	server.Pipeline.DB.Register(scope, opts...)
	server.Pipeline.Validate.Register(
		validate.UniqueField(rawDB, maniflex.SQLite, "slug"),
		maniflex.ForModel("HoistSite"))

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func hoistReq(t *testing.T, base, method, owner, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, base+"/api/hoist_sites", r)
	req.Header.Set("X-Owner", owner)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestScopeHoist_SingletonUniqueDoesNotConflictWithItself is the bug.
func TestScopeHoist_SingletonUniqueDoesNotConflictWithItself(t *testing.T) {
	base := hoistSrv(t, true)

	if code, body := hoistReq(t, base, "GET", "owner-a", ""); code != http.StatusOK {
		t.Fatalf("setup GET: %d %s", code, body)
	}
	// Re-send the row's own unchanged value. Only this row holds it.
	if code, body := hoistReq(t, base, "PATCH", "owner-a", `{"slug":"x"}`); code != http.StatusOK {
		t.Errorf("PATCH with the row's own slug: got %d, want 200 — %s", code, body)
	}
}

// TestScopeHoist_UniquenessStillEnforcedAcrossScopes is the anti-over-reach pair.
// A fix that resolved the id by simply dropping the exclusion, or by skipping the
// check for singletons, would let one owner take another owner's slug.
func TestScopeHoist_UniquenessStillEnforcedAcrossScopes(t *testing.T) {
	base := hoistSrv(t, true)

	if code, body := hoistReq(t, base, "GET", "owner-a", ""); code != http.StatusOK {
		t.Fatalf("setup a: %d %s", code, body)
	}
	if code, body := hoistReq(t, base, "PATCH", "owner-a", `{"slug":"taken"}`); code != http.StatusOK {
		t.Fatalf("setup a patch: %d %s", code, body)
	}
	if code, body := hoistReq(t, base, "GET", "owner-b", ""); code != http.StatusOK {
		t.Fatalf("setup b: %d %s", code, body)
	}
	// owner-b reaching for owner-a's slug must still be refused.
	if code, body := hoistReq(t, base, "PATCH", "owner-b", `{"slug":"taken"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("cross-scope duplicate slug: got %d, want 422 — %s", code, body)
	}
}

// TestScopeHoist_ScopeStillAppliesToTheQuery: hoisting moves when the middleware
// runs, and must not change what it does. Each owner still sees only its own row.
func TestScopeHoist_ScopeStillAppliesToTheQuery(t *testing.T) {
	base := hoistSrv(t, true)

	if code, _ := hoistReq(t, base, "PATCH", "owner-a", `{"slug":"a-slug"}`); code != http.StatusOK {
		t.Fatalf("provision a")
	}
	if code, _ := hoistReq(t, base, "PATCH", "owner-b", `{"slug":"b-slug"}`); code != http.StatusOK {
		t.Fatalf("provision b")
	}
	_, body := hoistReq(t, base, "GET", "owner-a", "")
	if !bytes.Contains([]byte(body), []byte("a-slug")) || bytes.Contains([]byte(body), []byte("b-slug")) {
		t.Errorf("owner-a's read is not scoped to its own row: %s", body)
	}
}

// TestScopeHoist_RunsExactlyOnce: compose() skips a ProvidesScope middleware in
// its own step because scopeChain runs it. If both fired, the forced filter would
// be appended twice — harmless to the result, so assert the count directly.
func TestScopeHoist_RunsExactlyOnce(t *testing.T) {
	calls := 0
	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(HoistSite{}, maniflex.ModelConfig{Singleton: true, TableName: "hoist_sites"})
	server.Pipeline.DB.Register(
		func(ctx *maniflex.ServerContext, next func() error) error {
			calls++
			return next()
		},
		maniflex.ForModel("HoistSite"), maniflex.ProvidesScope())

	rawDB, _ := sql.Open("sqlite", ":memory:")
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	hoistReq(t, ts.URL, "GET", "owner-a", "")
	if calls != 1 {
		t.Errorf("scope middleware ran %d times, want exactly 1", calls)
	}
}
