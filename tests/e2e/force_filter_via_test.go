package e2e

// R2 — db.ForceFilterVia scopes a model that carries no column to scope by,
// through the column its BelongsTo parent carries.
//
// Post has no `role` column; whether it is in scope is a fact about its User.
// That is the shape of every child table this exists for — a DamagedItem scoped
// by items.owner_id, a CartLine by carts.buyer_sub.
//
// The read/update/delete half rides on machinery that already worked: a nested
// FilterExpr joins, and P0-1's pre-flight passes the same filters to FindByID,
// which joins them too. What is new is the foreign key itself — the one part of
// the scope the client supplies, which nothing looked at before.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// viaSrv scopes Post through its author's role, read from an X-Role header.
func viaSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.ForceFilterVia("user", "role", func(ctx *maniflex.ServerContext) any {
					if r := ctx.Request.Header.Get("X-Role"); r != "" {
						return r
					}
					return nil
				}), maniflex.ForModel("Post"))
		},
	})
}

var (
	asAdmin  = map[string]string{"X-Role": "admin"}
	asEditor = map[string]string{"X-Role": "editor"}
)

// viaSeed returns a server plus one admin-authored and one editor-authored post.
func viaSeed(t *testing.T) (srv *testutil.Server, admin, editor, adminPost, editorPost string) {
	t.Helper()
	srv = viaSrv(t)
	admin = srv.MustID(srv.CreateUser("Ann", "ann@via.com", "admin"))
	editor = srv.MustID(srv.CreateUser("Ed", "ed@via.com", "editor"))
	adminPost = srv.MustID(srv.CreatePost("by-admin", "published", admin))
	editorPost = srv.MustID(srv.CreatePost("by-editor", "published", editor))
	return
}

func TestForceFilterVia_ScopesList(t *testing.T) {
	srv, _, _, _, _ := viaSeed(t)

	items := srv.GET("/posts", asAdmin).AssertStatus(http.StatusOK).DataList()
	if len(items) != 1 {
		t.Fatalf("list = %d items, want 1 — the scope did not join the parent", len(items))
	}
	if got := items[0].(map[string]any)["title"]; got != "by-admin" {
		t.Errorf("list returned %v, want by-admin", got)
	}
}

func TestForceFilterVia_ScopesRead(t *testing.T) {
	srv, _, _, adminPost, editorPost := viaSeed(t)

	srv.GET("/posts/"+adminPost, asAdmin).AssertStatus(http.StatusOK)
	srv.GET("/posts/"+editorPost, asAdmin).AssertStatus(http.StatusNotFound)
}

func TestForceFilterVia_ScopesUpdate(t *testing.T) {
	srv, _, _, _, editorPost := viaSeed(t)

	srv.PATCH("/posts/"+editorPost, map[string]any{"title": "PWNED"}, asAdmin).
		AssertStatus(http.StatusNotFound)

	// The row must be untouched, not merely the response unhelpful.
	after := srv.GET("/posts/"+editorPost, asEditor).AssertStatus(http.StatusOK)
	if got := after.Data()["title"]; got != "by-editor" {
		t.Errorf("title = %v, want by-editor — the cross-scope PATCH was refused but still wrote", got)
	}
}

func TestForceFilterVia_ScopesDelete(t *testing.T) {
	srv, _, _, _, editorPost := viaSeed(t)

	srv.DELETE("/posts/"+editorPost, asAdmin).AssertStatus(http.StatusNotFound)
	srv.GET("/posts/"+editorPost, asEditor).AssertStatus(http.StatusOK) // survives
}

// The create hole. Nothing reads a row that does not exist yet, so before the
// parent pre-flight this answered 201: an admin-scoped caller planted a Post
// under an editor, where the editor could see it and the admin could not.
func TestForceFilterVia_ScopesCreate(t *testing.T) {
	srv, _, editor, _, _ := viaSeed(t)

	srv.POST("/posts", map[string]any{
		"title": "planted", "body": "b", "status": "draft", "user_id": editor,
	}, asAdmin).AssertStatus(http.StatusNotFound)

	// And it must not have landed: the owner's own list is the only place such a
	// row would ever have shown up.
	for _, it := range srv.GET("/posts", asEditor).DataList() {
		if title := it.(map[string]any)["title"]; title == "planted" {
			t.Fatal("the cross-scope create was refused but still wrote the row")
		}
	}
}

// The reparent. enforceWriteScope reads the row back through the *old* foreign
// key, so a PATCH that rewrites it passes a check of where the row used to be.
func TestForceFilterVia_ScopesReparent(t *testing.T) {
	srv, _, editor, adminPost, _ := viaSeed(t)

	srv.PATCH("/posts/"+adminPost, map[string]any{"user_id": editor}, asAdmin).
		AssertStatus(http.StatusNotFound)

	// The admin's own post must still be theirs, not handed to the editor. Reading
	// it back as the admin is itself part of the assertion: had the reparent
	// landed, this read would 404 for them and succeed for the editor.
	after := srv.GET("/posts/"+adminPost, asAdmin).AssertStatus(http.StatusOK)
	if got := after.Data()["user_id"]; got == editor {
		t.Errorf("user_id = %v (the editor) — the reparent was refused but still wrote", got)
	}
}

// The other half: scoping must not break the writes it is meant to allow.
func TestForceFilterVia_InScopeWritesStillWork(t *testing.T) {
	srv, admin, _, adminPost, _ := viaSeed(t)

	srv.PATCH("/posts/"+adminPost, map[string]any{"title": "renamed"}, asAdmin).
		AssertStatus(http.StatusOK)
	if v := srv.GET("/posts/"+adminPost, asAdmin).Data()["title"]; v != "renamed" {
		t.Errorf("title = %v, want renamed — an in-scope update was refused or dropped", v)
	}

	created := srv.POST("/posts", map[string]any{
		"title": "mine", "body": "b", "status": "draft", "user_id": admin,
	}, asAdmin)
	created.AssertStatus(http.StatusCreated)

	// A create is only really in scope if its author can then read it back.
	srv.GET("/posts/"+created.ID(), asAdmin).AssertStatus(http.StatusOK)

	srv.DELETE("/posts/"+adminPost, asAdmin).AssertStatus(http.StatusNoContent)
	srv.GET("/posts/"+adminPost, asAdmin).AssertStatus(http.StatusNotFound)
}

// A create naming a parent that does not exist at all gets the same 404 as one
// naming a parent out of scope — the caller cannot see either, and telling them
// which is which is the disclosure the scoped read already refuses.
func TestForceFilterVia_CreateUnderMissingParent(t *testing.T) {
	srv := viaSrv(t)

	srv.POST("/posts", map[string]any{
		"title": "orphan", "body": "b", "status": "draft",
		"user_id": "00000000-0000-0000-0000-000000000000",
	}, asAdmin).AssertStatus(http.StatusNotFound)
}

// An unscoped request — no X-Role, so the resolver returns nil — behaves as it
// always did. This is the overwhelmingly common case and must cost nothing.
func TestForceFilterVia_UnscopedRequestUnaffected(t *testing.T) {
	srv, _, _, _, _ := viaSeed(t)

	if n := len(srv.GET("/posts").DataList()); n != 2 {
		t.Errorf("unscoped list = %d items, want 2 — a nil resolver still filtered", n)
	}
}

// Registering the scope on a model that has no such relation must fail the
// request, not serve it unscoped: an unenforceable scope is not a weaker scope.
func TestForceFilterVia_UnknownRelationFailsClosed(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.ForceFilterVia("nonexistent", "role", func(*maniflex.ServerContext) any {
					return "admin"
				}), maniflex.ForModel("Post"))
		},
	})
	resp := srv.GET("/posts")
	if resp.Status < 500 {
		t.Errorf("status = %d, want 5xx — an unresolvable scope served the request instead "+
			"of failing it", resp.Status)
	}
}

// A HasMany cannot carry a scope: the join needs a foreign key on this row, and
// a HasMany has none. Refuse rather than emit a predicate that means nothing.
func TestForceFilterVia_HasManyFailsClosed(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.ForceFilterVia("posts", "status", func(*maniflex.ServerContext) any {
					return "published"
				}), maniflex.ForModel("User"))
		},
	})
	resp := srv.GET("/users")
	if resp.Status < 500 {
		t.Errorf("status = %d, want 5xx — a HasMany scope served the request instead of "+
			"failing it", resp.Status)
	}
}
