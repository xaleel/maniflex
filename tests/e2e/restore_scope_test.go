package e2e

// Audit AUTH-10: a restore dispatches as OpUpdate against a soft-deleted row,
// and every read path hides that row — so the two middlewares that pre-fetch
// the target before deciding both got it wrong, in opposite directions.
//
//   - auth.Enforce read ErrNotFound as "the DB step will 404 this" and called
//     next(). The DB step's restore path *does* find the row, so any
//     authenticated caller could un-delete any record by id, and the response
//     handed back the whole row with it.
//   - auth.RequireOwner read it as "not yours" and answered 404 — to the
//     record's own owner. Restore worked for admins and nobody else.
//
// The same fetch serves the derived reads, and a third door was open there:
// the history query deliberately omits the soft-delete condition, so once a
// record was deleted, Enforce stopped guarding its diffs.
//
// db.ForceFilter was right all along, which is what the fix is built on: the
// restore statement applies forced filters through a subquery that omits the
// soft-delete condition. RequireOwner now imposes its ownership test the same
// way instead of fetching. Enforce cannot — a Policy is arbitrary Go over a
// record map, not a predicate — so it refuses a restore and says why.
//
//	go test ./tests/e2e/... -run TestRestoreScope

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	mdb "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type rscNote struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable"`
}

// rscCamelNote spells the owner column differently in JSON and in the database,
// and RequireOwner is given the DB name. The forced filter has to resolve under
// that spelling — AUTH-4 found the create stamp using the wrong one of the two.
type rscCamelNote struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"ownerId" db:"owner_id" mfx:"filterable"`
}

func rscServer(t *testing.T, reg func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			rscNote{}, maniflex.ModelConfig{RestoreEnabled: true, Versioned: true},
			rscCamelNote{}, maniflex.ModelConfig{RestoreEnabled: true},
		},
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
			reg(s)
		},
	})
}

// rscOwnerOnly allows only the record's owner.
func rscOwnerOnly(ctx *maniflex.ServerContext, rec map[string]any) (bool, error) {
	if ctx.Auth == nil {
		return false, nil
	}
	return rec["owner_id"] == ctx.Auth.UserID, nil
}

// seedDeleted creates a record owned by owner and soft-deletes it.
func seedDeleted(t *testing.T, srv *testutil.Server, path, ownerKey, owner string) string {
	t.Helper()
	id := srv.MustID(srv.POST(path,
		map[string]any{"title": "draft", ownerKey: owner}, asUser(owner)))
	srv.DELETE(path+"/"+id, asUser(owner)).AssertStatus(http.StatusNoContent)
	return id
}

// ── RequireOwner ─────────────────────────────────────────────────────────────

func TestRestoreScope_RequireOwnerLetsTheOwnerRestore(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("alice")).
		AssertStatus(http.StatusOK)
	// The row is really back, and still hers.
	srv.GET("/rsc_notes/"+id, asUser("alice")).AssertStatus(http.StatusOK).
		AssertJSON(func(b map[string]any) {
			d, ok := b["data"].(map[string]any)
			if !ok {
				t.Fatalf("no data object in the response: %v", b)
			}
			if d["owner_id"] != "alice" {
				t.Errorf("owner_id: got %v, want alice — a restore must not rewrite it", d["owner_id"])
			}
			if d["deleted_at"] != nil {
				t.Errorf("deleted_at: got %v, want nil", d["deleted_at"])
			}
		})
}

// MUST STILL WORK: the ownership test is the point. A non-owner is refused, and
// with the same 404 the fetch-and-compare path gave — now reached in SQL.
func TestRestoreScope_RequireOwnerRefusesANonOwner(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("bob")).
		AssertStatus(http.StatusNotFound)
	// And it really did not happen.
	srv.GET("/rsc_notes/"+id, asUser("alice")).AssertStatus(http.StatusNotFound)
}

// MUST STILL WORK: adminRoles bypass the switch entirely, as they do for the
// ownership handoff AUTH-4 closed.
func TestRestoreScope_RequireOwnerAdminStillRestores(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("root", "admin")).
		AssertStatus(http.StatusOK)
}

// The owner column's JSON and DB names differ and RequireOwner is given the DB
// name. A filter field that resolved under only one spelling would either refuse
// the owner or scope nothing at all; both directions are asserted.
func TestRestoreScope_RequireOwnerResolvesEitherColumnSpelling(t *testing.T) {
	t.Parallel()
	for _, given := range []string{"owner_id", "ownerId"} {
		t.Run(given, func(t *testing.T) {
			t.Parallel()
			srv := rscServer(t, func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.RequireOwner(given, "admin"))
			})
			id := seedDeleted(t, srv, "/rsc_camel_notes", "ownerId", "alice")

			srv.POST("/rsc_camel_notes/"+id+"/restore", nil, asUser("bob")).
				AssertStatus(http.StatusNotFound)
			srv.POST("/rsc_camel_notes/"+id+"/restore", nil, asUser("alice")).
				AssertStatus(http.StatusOK)
		})
	}
}

// MUST STILL WORK: an ordinary update is unaffected — it still fetches the row
// and compares, and still stamps the owner (AUTH-4). Only a restore takes the
// filter path.
func TestRestoreScope_RequireOwnerOrdinaryUpdateUnchanged(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := srv.MustID(srv.POST("/rsc_notes",
		map[string]any{"title": "live", "owner_id": "alice"}, asUser("alice")))

	srv.PATCH("/rsc_notes/"+id, map[string]any{"title": "edited"}, asUser("bob")).
		AssertStatus(http.StatusNotFound)
	srv.PATCH("/rsc_notes/"+id, map[string]any{"title": "edited", "owner_id": "bob"},
		asUser("alice")).AssertStatus(http.StatusOK)
	srv.GET("/rsc_notes/"+id, asUser("alice")).AssertStatus(http.StatusOK).
		AssertJSON(func(b map[string]any) {
			d, ok := b["data"].(map[string]any)
			if !ok {
				t.Fatalf("no data object in the response: %v", b)
			}
			if d["owner_id"] != "alice" {
				t.Errorf("owner_id: got %v, want alice — the handoff must stay closed", d["owner_id"])
			}
		})
}

// ── Enforce ──────────────────────────────────────────────────────────────────

// The High half: this used to answer 200 and hand back the restored row.
func TestRestoreScope_EnforceRefusesARestore(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(rscOwnerOnly))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	resp := srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("bob")).
		AssertStatus(http.StatusForbidden)
	if !strings.Contains(string(resp.Body), "db.ForceFilter") {
		t.Errorf("the refusal must name the mechanism that does scope a restore; got %s", resp.Body)
	}
	// The record stayed deleted — the refusal is not cosmetic.
	srv.GET("/rsc_notes/"+id, asUser("alice")).AssertStatus(http.StatusNotFound)
}

// The owner is refused too. That is the cost of failing closed: Enforce cannot
// read a deleted row, so it cannot tell the owner from anyone else.
func TestRestoreScope_EnforceRefusesTheOwnerToo(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(rscOwnerOnly))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("alice")).
		AssertStatus(http.StatusForbidden)
}

// The third door: deleting a record used to strip the policy off its history.
// Bob got 200 and alice's full field-level diffs, values included.
func TestRestoreScope_EnforceGuardsHistoryOfADeletedRecord(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(rscOwnerOnly))
	})
	id := srv.MustID(srv.POST("/rsc_notes",
		map[string]any{"title": "secret", "owner_id": "alice"}, asUser("alice")))
	srv.PATCH("/rsc_notes/"+id, map[string]any{"title": "secret v2"}, asUser("alice")).
		AssertStatus(http.StatusOK)

	// MUST STILL WORK: while the record is live, AUTH-2's guard answers.
	srv.GET("/rsc_notes/"+id+"/history", asUser("bob")).AssertStatus(http.StatusForbidden)
	srv.GET("/rsc_notes/"+id+"/history", asUser("alice")).AssertStatus(http.StatusOK)

	srv.DELETE("/rsc_notes/"+id, asUser("alice")).AssertStatus(http.StatusNoContent)

	resp := srv.GET("/rsc_notes/"+id+"/history", asUser("bob")).
		AssertStatus(http.StatusNotFound)
	if strings.Contains(string(resp.Body), "secret") {
		t.Errorf("the deleted record's diffs leaked in the refusal body: %s", resp.Body)
	}
}

// ── The mechanism the fix is built on ────────────────────────────────────────

// MUST STILL WORK: db.ForceFilter already scoped restores correctly, and is what
// the refusal above points operators at. If this ever stops holding, the advice
// in that message is wrong and RequireOwner's fix has lost its foundation.
func TestRestoreScope_ForceFilterScopesARestore(t *testing.T) {
	t.Parallel()
	srv := rscServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(mdb.ForceFilter("owner_id",
			func(ctx *maniflex.ServerContext) any { return ctx.Auth.UserID }))
	})
	id := seedDeleted(t, srv, "/rsc_notes", "owner_id", "alice")

	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("bob")).
		AssertStatus(http.StatusNotFound)
	srv.POST("/rsc_notes/"+id+"/restore", nil, asUser("alice")).
		AssertStatus(http.StatusOK)
}
