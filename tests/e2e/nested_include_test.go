package e2e_test

// 11D.2 — one level of nested includes: ?include=a.b.
//
// Scoped to the HTTP/JSON path deliberately. The typed companion structs still
// populate at depth 1 only; ?include= is a client-facing feature and the map
// path is what serves it.
//
// Depth is capped at two segments. a.b.c is refused at parse time rather than
// served, because query cost multiplies per level and an uncapped tree is a
// denial-of-service surface a client controls.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
	dbmw "github.com/xaleel/maniflex/middleware/db"
)

// niCompany ← niAuthor ← niPost, a two-hop BelongsTo chain.
type niCompany struct {
	maniflex.BaseModel
	Name    string `json:"name"`
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable"`
	Secret  string `json:"secret"   mfx:"hidden"`
}

type niAuthor struct {
	maniflex.BaseModel
	Name        string     `json:"name"`
	OwnerID     string     `json:"owner_id"    db:"owner_id"    mfx:"filterable"`
	NiCompanyID string     `json:"ni_company_id" db:"ni_company_id" mfx:"relation"`
	NiCompany   *niCompany `json:"ni_company,omitempty"`
}

type niPost struct {
	maniflex.BaseModel
	Title      string    `json:"title"`
	OwnerID    string    `json:"owner_id"   db:"owner_id"   mfx:"filterable"`
	NiAuthorID string    `json:"ni_author_id" db:"ni_author_id" mfx:"relation"`
	NiAuthor   *niAuthor `json:"ni_author,omitempty"`
}

// niTag / niPostTag give the HasMany + ManyToMany shapes a nested level too.
type niTag struct {
	maniflex.BaseModel
	Label   string `json:"label"`
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable"`
}

func niSrv(t *testing.T, scoped bool) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(niCompany{}, niAuthor{}, niPost{}, niTag{})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, srv.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	srv.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), srv.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if scoped {
		for _, m := range []string{"niCompany", "niAuthor", "niPost", "niTag"} {
			srv.Pipeline.DB.Register(
				dbmw.ForceFilter("owner_id", func(ctx *maniflex.ServerContext) any {
					if o := ctx.Request.Header.Get("X-Owner"); o != "" {
						return o
					}
					return nil
				}), maniflex.ForModel(m), maniflex.ProvidesScope())
		}
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func niReq(t *testing.T, base, method, path, owner, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, base+"/api"+path, r)
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
	var env map[string]any
	_ = json.Unmarshal(b, &env)
	if env == nil {
		env = map[string]any{"_raw": string(b)}
	}
	return resp.StatusCode, env
}

func niID(t *testing.T, env map[string]any) string {
	t.Helper()
	d, _ := env["data"].(map[string]any)
	id, _ := d["id"].(string)
	if id == "" {
		t.Fatalf("no id in %v", env)
	}
	return id
}

// niSeed builds company ← author ← post for one owner and returns the post id.
func niSeed(t *testing.T, base, owner string) string {
	t.Helper()
	_, c := niReq(t, base, "POST", "/ni_companies", owner,
		`{"name":"Acme Ltd","owner_id":"`+owner+`","secret":"classified"}`)
	cid := niID(t, c)
	_, a := niReq(t, base, "POST", "/ni_authors", owner,
		`{"name":"Ada","owner_id":"`+owner+`","ni_company_id":"`+cid+`"}`)
	aid := niID(t, a)
	_, p := niReq(t, base, "POST", "/ni_posts", owner,
		`{"title":"Hello","owner_id":"`+owner+`","ni_author_id":"`+aid+`"}`)
	return niID(t, p)
}

// TestNestedInclude_TwoLevels is the feature.
func TestNestedInclude_TwoLevels(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	code, env := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author.ni_company", "", "")
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	author, ok := data["ni_author"].(map[string]any)
	if !ok {
		t.Fatalf("ni_author missing: %v", data)
	}
	company, ok := author["ni_company"].(map[string]any)
	if !ok {
		t.Fatalf("nested ni_company missing from the author: %v", author)
	}
	if company["name"] != "Acme Ltd" {
		t.Errorf("nested company name = %v, want Acme Ltd", company["name"])
	}
}

// TestNestedInclude_ImpliesTheParent: asking only for a.b must still emit a,
// since there is nowhere else to hang b.
func TestNestedInclude_ImpliesTheParent(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	_, env := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author.ni_company", "", "")
	data, _ := env["data"].(map[string]any)
	if _, ok := data["ni_author"].(map[string]any); !ok {
		t.Errorf("include=a.b must also include a; got %v", data)
	}
}

// TestNestedInclude_HiddenFieldsStrippedAtDepth: the response serialiser already
// recurses, and must keep doing so — a hidden field on the *nested* model must
// not appear just because it arrived one level down.
func TestNestedInclude_HiddenFieldsStrippedAtDepth(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	_, env := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author.ni_company", "", "")
	data, _ := env["data"].(map[string]any)
	author, _ := data["ni_author"].(map[string]any)
	company, ok := author["ni_company"].(map[string]any)
	// Assert presence first: this test checks that a key is *absent*, so without
	// this it would pass for the wrong reason if the nested include broke.
	if !ok {
		t.Fatalf("nested company missing — the assertion below would be vacuous: %v", author)
	}
	if company["name"] != "Acme Ltd" {
		t.Fatalf("nested company is not the expected row: %v", company)
	}
	if _, leaked := company["secret"]; leaked {
		t.Errorf(`mfx:"hidden" field surfaced on a depth-2 include: %v`, company)
	}
}

// TestNestedInclude_ScopedAtEveryLevel is the security case. A forced filter
// scopes the root and the first level already; it must reach the second too, or
// ?include=a.b becomes a way to read another tenant's rows.
func TestNestedInclude_ScopedAtEveryLevel(t *testing.T) {
	base := niSrv(t, true)
	niSeed(t, base, "ada")

	// bob owns a post whose author points at *ada's* company — the FK is the
	// client's to set, so this is reachable.
	_, c := niReq(t, base, "POST", "/ni_companies", "ada", `{"name":"Acme Ltd","owner_id":"ada"}`)
	adaCompany := niID(t, c)
	_, a := niReq(t, base, "POST", "/ni_authors", "bob",
		`{"name":"Bob","owner_id":"bob","ni_company_id":"`+adaCompany+`"}`)
	bobAuthor := niID(t, a)
	_, p := niReq(t, base, "POST", "/ni_posts", "bob",
		`{"title":"Bob post","owner_id":"bob","ni_author_id":"`+bobAuthor+`"}`)
	bobPost := niID(t, p)

	_, env := niReq(t, base, "GET", "/ni_posts/"+bobPost+"?include=ni_author.ni_company", "bob", "")
	data, _ := env["data"].(map[string]any)
	author, ok := data["ni_author"].(map[string]any)
	// Bob's own author must be there, or the absence below proves nothing.
	if !ok {
		t.Fatalf("bob's own author missing — the assertion below would be vacuous: %v", data)
	}
	if author["name"] != "Bob" {
		t.Fatalf("unexpected author: %v", author)
	}
	if company, present := author["ni_company"].(map[string]any); present {
		t.Errorf("bob read ada's company through a nested include: %v", company)
	}
}

// TestNestedInclude_DepthLimit: three segments is refused, and the message says
// why rather than reporting an unknown relation.
func TestNestedInclude_DepthLimit(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	code, env := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author.ni_company.owner", "", "")
	if code != http.StatusBadRequest {
		t.Fatalf("a 3-deep include should be refused, got %d: %v", code, env)
	}
	blob, _ := json.Marshal(env)
	if !bytes.Contains(blob, []byte("deep")) {
		t.Errorf("the error should explain the depth limit, got %s", blob)
	}
}

// TestNestedInclude_UnknownNestedRelationRejected: a typo in the second segment
// must be an error, not a silently absent key.
func TestNestedInclude_UnknownNestedRelationRejected(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	code, _ := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author.nope", "", "")
	if code != http.StatusBadRequest {
		t.Errorf("unknown nested relation should be 400, got %d", code)
	}
}

// TestNestedInclude_FlatStillWorks is the anti-over-reach pair.
func TestNestedInclude_FlatStillWorks(t *testing.T) {
	base := niSrv(t, false)
	pid := niSeed(t, base, "ada")

	_, env := niReq(t, base, "GET", "/ni_posts/"+pid+"?include=ni_author", "", "")
	data, _ := env["data"].(map[string]any)
	author, ok := data["ni_author"].(map[string]any)
	if !ok {
		t.Fatalf("flat include broke: %v", data)
	}
	if author["name"] != "Ada" {
		t.Errorf("author name = %v, want Ada", author["name"])
	}
	// A flat include must NOT drag the next level along.
	if _, over := author["ni_company"]; over {
		t.Errorf("include=a pulled in a.b as well: %v", author)
	}
}

// TestNestedInclude_ListEndpoint: the nested level must batch across all rows of
// a list, not just work for a single read.
func TestNestedInclude_ListEndpoint(t *testing.T) {
	base := niSrv(t, false)
	niSeed(t, base, "ada")
	niSeed(t, base, "ada")

	code, env := niReq(t, base, "GET", "/ni_posts?include=ni_author.ni_company", "", "")
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, env)
	}
	list, _ := env["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(list))
	}
	for i, row := range list {
		r, _ := row.(map[string]any)
		author, _ := r["ni_author"].(map[string]any)
		if _, ok := author["ni_company"].(map[string]any); !ok {
			t.Errorf("row %d: nested company missing: %v", i, author)
		}
	}
}
