package e2e

// Audit AUTH-4: on update, RequireOwner and Enforce checked the row as stored
// and let the body through unconstrained, so an owner could PATCH the owner
// column and hand the record to someone else — under RequireOwner, locking
// themselves out of it on the next read.
//
// RequireOwner now stamps on update as it does on create. Enforce is unchanged
// and documented instead: its update policies see the stored row, and the docs'
// worked example for checking the proposed owner is pinned below.
//
//	go test ./tests/e2e/... -run TestOwnerUpdate

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type updOwnedNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id"`
}

type updImmutableNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"immutable"`
}

// The owner column's JSON name differs from its DB column, and RequireOwner is
// given the column. Its doc has always allowed that; only the ownership check
// honoured it, while the stamp used the argument as a JSON name.
type updCamelNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"ownerId" db:"owner_id"`
}

func updOwnerServer(t *testing.T, model any) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{model},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				uid := ctx.Request.Header.Get("X-Test-User")
				if uid == "" {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
					return nil
				}
				info := &maniflex.AuthInfo{UserID: uid}
				if r := ctx.Request.Header.Get("X-Test-Role"); r != "" {
					info.Roles = []string{r}
				}
				ctx.Auth = info
				return next()
			})
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
		},
	})
}

func TestOwnerUpdate_OwnerCannotHandTheRecordOff(t *testing.T) {
	t.Parallel()
	srv := updOwnerServer(t, updOwnedNote{})
	alice, bob := asUser("alice"), asUser("bob")
	id := srv.MustID(srv.POST("/upd_owned_notes", map[string]any{"title": "mine"}, alice))

	resp := srv.PATCH("/upd_owned_notes/"+id,
		map[string]any{"title": "renamed", "owner_id": "bob"}, alice)
	// Must still work: the update itself goes through. Only the owner column is
	// held, the way a readonly field is — silently.
	resp.AssertStatus(http.StatusOK)
	if got := resp.Data()["title"]; got != "renamed" {
		t.Errorf("the rest of the update was lost: title %v", got)
	}
	if got := resp.Data()["owner_id"]; got != "alice" {
		t.Errorf("update answered owner_id %v, want alice", got)
	}

	// And as stored: alice keeps it, bob still cannot see it.
	srv.GET("/upd_owned_notes/"+id, alice).AssertStatus(http.StatusOK)
	srv.GET("/upd_owned_notes/"+id, bob).AssertStatus(http.StatusNotFound)
}

// adminRoles bypass RequireOwner before any of it runs, stamp included, so a
// transfer is still possible — just no longer by the owner acting alone.
func TestOwnerUpdate_AdminCanStillTransfer(t *testing.T) {
	t.Parallel()
	srv := updOwnerServer(t, updOwnedNote{})
	id := srv.MustID(srv.POST("/upd_owned_notes", map[string]any{"title": "t"}, asUser("alice")))

	srv.PATCH("/upd_owned_notes/"+id, map[string]any{"owner_id": "bob"}, asUser("root", "admin")).
		AssertStatus(http.StatusOK)

	srv.GET("/upd_owned_notes/"+id, asUser("bob")).AssertStatus(http.StatusOK)
	srv.GET("/upd_owned_notes/"+id, asUser("alice")).AssertStatus(http.StatusNotFound)
}

// Must still work: an immutable owner column is already protected on update and
// is not stamped. Updates to the rest of the row are unaffected either way.
func TestOwnerUpdate_ImmutableOwnerColumn(t *testing.T) {
	t.Parallel()
	srv := updOwnerServer(t, updImmutableNote{})
	alice := asUser("alice")
	id := srv.MustID(srv.POST("/upd_immutable_notes", map[string]any{"title": "t"}, alice))

	resp := srv.PATCH("/upd_immutable_notes/"+id,
		map[string]any{"title": "renamed", "owner_id": "bob"}, alice)
	resp.AssertStatus(http.StatusOK)
	if got := resp.Data()["title"]; got != "renamed" {
		t.Errorf("title %v, want renamed", got)
	}
	srv.GET("/upd_immutable_notes/"+id, alice).AssertStatus(http.StatusOK)
	srv.GET("/upd_immutable_notes/"+id, asUser("bob")).AssertStatus(http.StatusNotFound)
}

// RequireOwner("owner_id") on a field whose JSON name is ownerId: the stamp went
// to a body key the model does not have, so the column was written empty — the
// record unreadable by the user who created it — and a client sending "ownerId"
// chose the owner, on create or on update.
func TestOwnerUpdate_OwnerFieldGivenByColumnName(t *testing.T) {
	t.Parallel()
	srv := updOwnerServer(t, updCamelNote{})
	alice, bob := asUser("alice"), asUser("bob")

	omitted := srv.MustID(srv.POST("/upd_camel_notes", map[string]any{"title": "t"}, alice))
	srv.GET("/upd_camel_notes/"+omitted, alice).AssertStatus(http.StatusOK)

	planted := srv.MustID(srv.POST("/upd_camel_notes",
		map[string]any{"title": "t", "ownerId": "bob"}, alice))
	srv.GET("/upd_camel_notes/"+planted, alice).AssertStatus(http.StatusOK)
	srv.GET("/upd_camel_notes/"+planted, bob).AssertStatus(http.StatusNotFound)

	srv.PATCH("/upd_camel_notes/"+omitted, map[string]any{"ownerId": "bob"}, alice).
		AssertStatus(http.StatusOK)
	srv.GET("/upd_camel_notes/"+omitted, alice).AssertStatus(http.StatusOK)
	srv.GET("/upd_camel_notes/"+omitted, bob).AssertStatus(http.StatusNotFound)
}

// The worked example from the Enforce docs, verbatim. Enforce itself was left
// alone — its update policies see the stored row — so what stops a handoff there
// is a policy written this way, and a docs example that did not work would be
// worse than none.
func TestOwnerUpdate_EnforceDocsExampleRefusesAHandoff(t *testing.T) {
	t.Parallel()
	ownerOnly := func(ctx *maniflex.ServerContext, r map[string]any) (bool, error) {
		if r["owner_id"] != ctx.Auth.UserID {
			return false, nil // not your record
		}
		// r is the row as stored. Refuse an update that would give it away.
		if ctx.Operation == maniflex.OpUpdate {
			if to, ok := ctx.Field("owner_id"); ok && to != ctx.Auth.UserID {
				return false, nil
			}
		}
		return true, nil
	}
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{updOwnedNote{}},
		Middleware: func(s *maniflex.Server) {
			testIdentity(s)
			s.Pipeline.DB.Register(auth.Enforce(ownerOnly))
		},
	})
	alice := asUser("alice")
	id := srv.MustID(srv.POST("/upd_owned_notes",
		map[string]any{"title": "t", "owner_id": "alice"}, alice))

	srv.PATCH("/upd_owned_notes/"+id, map[string]any{"owner_id": "bob"}, alice).
		AssertStatus(http.StatusForbidden)
	// Must still work: an ordinary edit by the owner, and one that restates them.
	srv.PATCH("/upd_owned_notes/"+id, map[string]any{"title": "renamed"}, alice).
		AssertStatus(http.StatusOK)
	srv.PATCH("/upd_owned_notes/"+id, map[string]any{"owner_id": "alice"}, alice).
		AssertStatus(http.StatusOK)
	srv.GET("/upd_owned_notes/"+id, asUser("bob")).AssertStatus(http.StatusForbidden)
}
