package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// OwnedNote is owned by the user whose id is stored in owner_id.
type OwnedNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id"`
}

// asUser builds request headers carrying a test identity (and optional role).
func asUser(userID string, roles ...string) map[string]string {
	h := map[string]string{"X-Test-User": userID}
	if len(roles) > 0 {
		h["X-Test-Role"] = roles[0]
	}
	return h
}

func requireOwnerServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{OwnedNote{}},
		Middleware: func(s *maniflex.Server) {
			// Test auth: identity from X-Test-User, role from X-Test-Role.
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

// SEC-3: RequireOwner must enforce ownership on read/update/delete, not just
// inject the owner on create. A non-owner must not be able to read, modify, or
// delete another user's record (IDOR).
func TestRequireOwner_EnforcesOnReadUpdateDelete(t *testing.T) {
	t.Parallel()
	srv := requireOwnerServer(t)

	alice := asUser("alice")
	bob := asUser("bob")

	// Alice creates a note; RequireOwner injects owner_id = "alice".
	created := srv.POST("/owned_notes", map[string]any{"title": "secret plans"}, alice).
		AssertStatus(http.StatusCreated).Data()
	if created["owner_id"] != "alice" {
		t.Fatalf("owner_id on create: got %v, want alice", created["owner_id"])
	}
	id := created["id"].(string)

	// Bob (not the owner) is denied read/update/delete. 404 (not 403) so the
	// endpoint does not reveal that the record exists.
	srv.GET("/owned_notes/"+id, bob).AssertStatus(http.StatusNotFound)
	srv.PATCH("/owned_notes/"+id, map[string]any{"title": "hacked"}, bob).AssertStatus(http.StatusNotFound)
	srv.DELETE("/owned_notes/"+id, bob).AssertStatus(http.StatusNotFound)

	// The record is untouched: Alice still reads the original title (which also
	// proves the record exists, so Bob's 404s were ownership denials).
	got := srv.GET("/owned_notes/"+id, alice).AssertStatus(http.StatusOK).Data()
	if got["title"] != "secret plans" {
		t.Errorf("title after Bob's attempts: got %v, want 'secret plans'", got["title"])
	}

	// Alice (the owner) can update and delete.
	srv.PATCH("/owned_notes/"+id, map[string]any{"title": "updated"}, alice).AssertStatus(http.StatusOK)
	srv.DELETE("/owned_notes/"+id, alice).AssertStatus(http.StatusNoContent)
}

// Users with an admin role bypass the ownership check on read/update/delete.
func TestRequireOwner_AdminBypassesOwnership(t *testing.T) {
	t.Parallel()
	srv := requireOwnerServer(t)

	alice := asUser("alice")
	admin := asUser("root", "admin")

	created := srv.POST("/owned_notes", map[string]any{"title": "t"}, alice).
		AssertStatus(http.StatusCreated).Data()
	id := created["id"].(string)

	// Admin is not the owner but may read, update, and delete.
	srv.GET("/owned_notes/"+id, admin).AssertStatus(http.StatusOK)
	srv.PATCH("/owned_notes/"+id, map[string]any{"title": "by admin"}, admin).AssertStatus(http.StatusOK)
	srv.DELETE("/owned_notes/"+id, admin).AssertStatus(http.StatusNoContent)
}
