package e2e_test

// Hoisting Tenancy is not the same as hoisting ForceFilter. Tenancy does two
// things: it appends a forced filter (movable) and it injects the tenant value
// into the write body (not obviously movable). Its own godoc notes that the
// Validate step strips immutable fields "before this middleware runs" — an
// ordering that ProvidesScope() inverts.
//
// These pin what hoisted Tenancy must still do.

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
)

// TenantDoc carries a readonly tenant column — readonly because a client must
// not set it, which is the ordinary way to declare a server-stamped field.
type TenantDoc struct {
	maniflex.BaseModel
	OrgID string `json:"org_id" db:"org_id" mfx:"filterable,readonly"`
	Title string `json:"title"  db:"title"`
}

func tenancySrv(t *testing.T, hoisted bool) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(TenantDoc{})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	opts := []maniflex.MiddlewareOption{maniflex.ForModel("TenantDoc")}
	if hoisted {
		opts = append(opts, maniflex.ProvidesScope())
	}
	server.Pipeline.DB.Register(dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
		return ctx.Request.Header.Get("X-Org")
	}), opts...)

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func orgReq(t *testing.T, base, method, org, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, base+"/api/tenant_docs", r)
	req.Header.Set("X-Org", org)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// TestScopeHoist_TenancyStillStampsCreates: the injection half must survive
// hoisting. If Validate strips the injected readonly field, the row is written
// with an empty org_id — invisible to its own tenant, and visible to a tenant
// whose id is also empty.
func TestScopeHoist_TenancyStillStampsCreates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hoisted bool
	}{{"conventional", false}, {"hoisted", true}} {
		t.Run(tc.name, func(t *testing.T) {
			base := tenancySrv(t, tc.hoisted)

			code, body := orgReq(t, base, "POST", "acme", `{"title":"Q3 plan"}`)
			if code != http.StatusCreated {
				t.Fatalf("create: %d %s", code, body)
			}

			// The tenant must be able to read back what it just wrote.
			code, body = orgReq(t, base, "GET", "acme", "")
			if code != http.StatusOK {
				t.Fatalf("list: %d %s", code, body)
			}
			if !contains(body, "Q3 plan") {
				t.Errorf("the created row is not visible to its own tenant — "+
					"org_id was not stamped: %s", body)
			}
			// And another tenant must not see it.
			if _, other := orgReq(t, base, "GET", "other", ""); contains(other, "Q3 plan") {
				t.Errorf("row leaked across tenants: %s", other)
			}
		})
	}
}
