package e2e_test

// 13.8 — a flat db.ForceFilter must stamp the scope onto writes, not only filter
// reads.
//
// ForceFilter appended a FilterExpr and nothing else. So a create under a scope
// stored whatever the client sent in the scope column — usually nothing — and the
// row landed outside its own author's scope: invisible to them on the very next
// read, and visible to anyone whose scope value is also empty. An update could
// rewrite the column outright and push the row into someone else's scope.
//
// Its three siblings all already do this: Tenancy stamps via ctx.SetField,
// ActionScope and scoped-singleton provisioning both via scopeColumns.

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

// ScopedNote carries a client-settable scope column — the shape that makes the
// leak reachable rather than merely lossy.
type ScopedNote struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable"`
	Body    string `json:"body"     db:"body"`
}

func stampSrv(t *testing.T) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(ScopedNote{})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	server.Pipeline.DB.Register(
		dbmw.ForceFilter("owner_id", func(ctx *maniflex.ServerContext) any {
			if o := ctx.Request.Header.Get("X-Owner"); o != "" {
				return o
			}
			return nil
		}),
		maniflex.ForModel("ScopedNote"), maniflex.ProvidesScope())

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func noteReq(t *testing.T, base, method, path, owner, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, base+"/api/scoped_notes"+path, r)
	if owner != "" {
		req.Header.Set("X-Owner", owner)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestForceFilterStamp_CreateIsVisibleToItsAuthor is the loss half: a row its own
// author cannot see is the minimum symptom, present even when the client sends
// nothing for the scope column.
func TestForceFilterStamp_CreateIsVisibleToItsAuthor(t *testing.T) {
	base := stampSrv(t)

	if code, body := noteReq(t, base, "POST", "", "ada", `{"body":"mine"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	code, body := noteReq(t, base, "GET", "", "ada", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	if !contains(body, "mine") {
		t.Errorf("the row is not visible to the caller who created it — "+
			"owner_id was not stamped: %s", body)
	}
}

// TestForceFilterStamp_CreateCannotTargetAnotherScope is the leak half: the scope
// column is the client's to send, so an unstamped create lets a caller plant a
// row directly inside someone else's scope.
func TestForceFilterStamp_CreateCannotTargetAnotherScope(t *testing.T) {
	base := stampSrv(t)

	code, body := noteReq(t, base, "POST", "", "mallory", `{"body":"planted","owner_id":"ada"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	// Whatever the response said, ada must not be holding mallory's row.
	if _, ada := noteReq(t, base, "GET", "", "ada", ""); contains(ada, "planted") {
		t.Errorf("mallory planted a row into ada's scope: %s", ada)
	}
}

// TestForceFilterStamp_UpdateCannotMoveRowOutOfScope: the forced filter stops an
// update *reaching* a row outside scope, but nothing stopped the update rewriting
// the scope column of a row inside it, pushing it out.
func TestForceFilterStamp_UpdateCannotMoveRowOutOfScope(t *testing.T) {
	base := stampSrv(t)

	_, created := noteReq(t, base, "POST", "", "ada", `{"body":"mine"}`)
	id := idFrom(t, created)

	if code, body := noteReq(t, base, "PATCH", "/"+id, "ada", `{"owner_id":"mallory"}`); code >= 400 {
		t.Fatalf("patch: %d %s", code, body)
	}
	if _, ada := noteReq(t, base, "GET", "", "ada", ""); !contains(ada, "mine") {
		t.Errorf("the row left its author's scope on update: %s", ada)
	}
}

// TestForceFilterStamp_ScopeStillFilters is the anti-over-reach pair: stamping
// must not weaken the read scoping it accompanies.
func TestForceFilterStamp_ScopeStillFilters(t *testing.T) {
	base := stampSrv(t)

	noteReq(t, base, "POST", "", "ada", `{"body":"ada-note"}`)
	noteReq(t, base, "POST", "", "bob", `{"body":"bob-note"}`)

	_, ada := noteReq(t, base, "GET", "", "ada", "")
	if !contains(ada, "ada-note") || contains(ada, "bob-note") {
		t.Errorf("read scoping broke: %s", ada)
	}
}

func idFrom(t *testing.T, body string) string {
	t.Helper()
	const key = `"id":"`
	i := bytes.Index([]byte(body), []byte(key))
	if i < 0 {
		t.Fatalf("no id in %s", body)
	}
	rest := body[i+len(key):]
	j := bytes.IndexByte([]byte(rest), '"')
	return rest[:j]
}
