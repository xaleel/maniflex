package e2e

// Audit PIPE-4 — a cascade writes children with the adapter directly
// (exec.Delete / exec.Update), which is what makes it one data operation rather
// than N requests. The child's own DB-step middleware therefore never ran, and
// versioning and rollups both live exactly there: a cascaded child left no
// history row, and the rollup that summarises it kept counting rows that were
// gone — drifting from a total Rollup's own documentation calls correct by
// construction, curable only by BackfillRollups.
//
// The gap had a second half the entry did not mention. When neither side
// soft-deletes, the edge used to be left to a database FK constraint's ON DELETE
// clause, which removes the rows and tells nobody — so the same drift happened
// there with the maniflex cascade never running at all. dbEnforcedDelete now
// draws the line at bookkeeping too: the framework enforces an edge itself
// whenever it has something to do beyond deleting the row.
//
// The history assertions read the table directly. They have to: /history reads
// the parent record first (MS-4), so it answers 404 for any deleted record —
// a directly deleted one included — and cannot show what this is about.

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type cbUser struct {
	maniflex.BaseModel
	Name         string `json:"name"`
	CommentCount int64  `json:"comment_count" db:"comment_count"`
}

// cbPost soft-deletes, so this edge was always enforced in the maniflex layer —
// the half the audit found.
type cbPost struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title string `json:"title"`
}

// cbBoard hard-deletes. Before the fix its edge went to a database FK
// constraint and the maniflex cascade never saw it.
type cbBoard struct {
	maniflex.BaseModel
	Title string `json:"title"`
}

// cbComment is versioned and rolls up into cbUser.comment_count. It cascades
// from cbPost and from cbBoard, and nulls its board pointer is not used — the
// setNull case has its own models below.
type cbComment struct {
	maniflex.BaseModel
	Body      string  `json:"body"`
	CbPostID  *string `json:"cb_post_id"  db:"cb_post_id"  mfx:"relation:CbPost;onDelete:cascade"`
	CbPost    cbPost  `json:"-"`
	CbBoardID *string `json:"cb_board_id" db:"cb_board_id" mfx:"relation:CbBoard;onDelete:cascade"`
	CbBoard   cbBoard `json:"-"`
	CbUserID  string  `json:"cb_user_id"  db:"cb_user_id"  mfx:"relation:CbUser"`
	CbUser    cbUser  `json:"-"`
}

func cbSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			cbUser{}, cbPost{}, cbBoard{},
			cbComment{}, maniflex.ModelConfig{Versioned: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
				maniflex.ForModel("cbComment"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "cbUser", ParentField: "comment_count", Op: maniflex.AggCount,
				Child: "cbComment", On: "cb_user_id",
			})
		},
	})
}

// historyOps returns the operations recorded for one record, in version order.
// The API cannot serve this for a deleted record, so the table is read directly.
func historyOps(t *testing.T, srv *testutil.Server, table, recordID string) []string {
	t.Helper()
	mfx := srv.ManiflexServer()
	bg := maniflex.NewBackground(context.Background(), mfx.DB(), mfx.Registry())
	rows, err := bg.RawQuery(
		"SELECT operation FROM "+table+" WHERE record_id = ? ORDER BY version", recordID)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	ops := make([]string, 0, len(rows))
	for _, r := range rows {
		ops = append(ops, r["operation"].(string))
	}
	return ops
}

func cbCount(t *testing.T, srv *testutil.Server, userID string) float64 {
	t.Helper()
	v := srv.GET("/cb_users/" + userID).AssertStatus(http.StatusOK).Data()["comment_count"]
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("comment_count is not numeric: %#v", v)
	}
	return f
}

// A cascaded child must end up indistinguishable from one deleted directly: the
// same history, and the same effect on the total that summarises it.
func TestCascade_ChildKeepsItsHistoryAndRollup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent string // the route whose row is deleted
		field  string // the FK on the comment naming it
	}{
		// Soft-deleting parent: always went through the maniflex walk.
		{"soft_delete_parent", "cb_posts", "cb_post_id"},
		// Hard-deleting parent: used to be left to a database FK constraint,
		// where none of this could run.
		{"hard_delete_parent", "cb_boards", "cb_board_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := cbSrv(t)
			uid := srv.MustID(srv.POST("/cb_users", map[string]any{"name": "Ada"}))

			// The reference answer: the same model, deleted directly.
			direct := srv.MustID(srv.POST("/cb_comments",
				map[string]any{"body": "direct", "cb_user_id": uid}))
			srv.DELETE("/cb_comments/" + direct).AssertStatus(http.StatusNoContent)
			wantOps := historyOps(t, srv, "cb_comment_history", direct)
			if len(wantOps) != 2 || wantOps[1] != "delete" {
				t.Fatalf("a direct delete recorded %v; the comparison below is meaningless otherwise", wantOps)
			}

			pid := srv.MustID(srv.POST("/"+tc.parent, map[string]any{"title": "P"}))
			cid := srv.MustID(srv.POST("/cb_comments",
				map[string]any{"body": "cascaded", "cb_user_id": uid, tc.field: pid}))
			if got := cbCount(t, srv, uid); got != 1 {
				t.Fatalf("comment_count = %v before the cascade, want 1", got)
			}

			srv.DELETE("/" + tc.parent + "/" + pid).AssertStatus(http.StatusNoContent)

			if got := srv.GET("/cb_comments/" + cid).Status; got != http.StatusNotFound {
				t.Errorf("the comment survived the cascade (%d) — the fixture is wrong", got)
			}
			if got := historyOps(t, srv, "cb_comment_history", cid); len(got) != len(wantOps) ||
				got[len(got)-1] != "delete" {
				t.Errorf("cascaded child recorded %v, want %v — the audit trail has a hole "+
					"precisely on a bulk destructive operation", got, wantOps)
			}
			if got := cbCount(t, srv, uid); got != 0 {
				t.Errorf("comment_count = %v after the child was cascaded away, want 0", got)
			}
		})
	}
}

// ── setNull ──────────────────────────────────────────────────────────────────

// cbOrg soft-deletes, so its row survives its own deletion and the rollup column
// on it stays readable — which is what makes the assertion below possible.
type cbOrg struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Name     string `json:"name"`
	DocCount int64  `json:"doc_count" db:"doc_count"`
}

// cbDoc nulls its org FK when the org goes, and that column is what the rollup
// keys on — so the null must move the doc out of the org's total.
type cbDoc struct {
	maniflex.BaseModel
	Title   string  `json:"title"`
	CbOrgID *string `json:"cb_org_id" db:"cb_org_id" mfx:"relation:CbOrg;onDelete:setNull"`
	CbOrg   cbOrg   `json:"-"`
}

// cbColumn reads one column straight from a table, for a row the API will not
// serve — here a soft-deleted org, which every read path hides.
func cbColumn(t *testing.T, srv *testutil.Server, table, col, id string) any {
	t.Helper()
	mfx := srv.ManiflexServer()
	bg := maniflex.NewBackground(context.Background(), mfx.DB(), mfx.Registry())
	rows, err := bg.RawQuery("SELECT "+col+" AS v FROM "+table+" WHERE id = ?", id)
	if err != nil {
		t.Fatalf("read %s.%s: %v", table, col, err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s has %d rows with id %s, want 1", table, len(rows), id)
	}
	return rows[0]["v"]
}

func TestCascade_SetNullRecordsAnUpdateAndMovesTheRollup(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			cbOrg{},
			cbDoc{}, maniflex.ModelConfig{Versioned: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
				maniflex.ForModel("cbDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
			// Keyed on the very column setNull writes to.
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "cbOrg", ParentField: "doc_count", Op: maniflex.AggCount,
				Child: "cbDoc", On: "cb_org_id",
			})
		},
	})

	oid := srv.MustID(srv.POST("/cb_orgs", map[string]any{"name": "Acme"}))
	did := srv.MustID(srv.POST("/cb_docs", map[string]any{"title": "Doc", "cb_org_id": oid}))
	if got := srv.GET("/cb_orgs/" + oid).AssertStatus(http.StatusOK).Data()["doc_count"]; got != float64(1) {
		t.Fatalf("doc_count = %#v before the cascade, want 1", got)
	}

	srv.DELETE("/cb_orgs/" + oid).AssertStatus(http.StatusNoContent)

	// The doc survives with a null FK, and that is an update it should have
	// recorded — the same as a PATCH clearing the field would.
	data := srv.GET("/cb_docs/" + did).AssertStatus(http.StatusOK).Data()
	if data["cb_org_id"] != nil {
		t.Errorf("cb_org_id = %#v after setNull, want null — the fixture is wrong", data["cb_org_id"])
	}
	if ops := historyOps(t, srv, "cb_doc_history", did); len(ops) != 2 || ops[1] != "update" {
		t.Errorf("setNull recorded %v, want a create then an update", ops)
	}
	// The org is soft-deleted, so it is still there to be wrong.
	if got := cbColumn(t, srv, "cb_orgs", "doc_count", oid); got != int64(0) {
		t.Errorf("doc_count = %#v after its only doc was nulled away, want 0", got)
	}
}

// ── the rollup parent the sweep itself deletes ───────────────────────────────

type cbAuthor struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Name string `json:"name"`
}

type cbArticle struct {
	maniflex.BaseModel
	Title     string   `json:"title"`
	NoteCount int64    `json:"note_count" db:"note_count"`
	CbAuthorI string   `json:"cb_author_i" db:"cb_author_i" mfx:"relation:CbAuthor;onDelete:cascade"`
	CbAuthor  cbAuthor `json:"-"`
}

type cbNote struct {
	maniflex.BaseModel
	Body       string    `json:"body"`
	CbArticleI string    `json:"cb_article_i" db:"cb_article_i" mfx:"relation:CbArticle;onDelete:cascade"`
	CbArticle  cbArticle `json:"-"`
}

// Author → Article → Note, with Note rolling up into Article. Deleting the
// author invalidates an article's column and then deletes that same article, so
// the deferred recompute targets a row that is gone. It must be skipped, not
// turned into a 500 on an ordinary delete.
func TestCascade_RollupParentDeletedInTheSameSweepIsSkipped(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{cbAuthor{}, cbArticle{}, cbNote{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
				maniflex.ForModel("cbNote"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "cbArticle", ParentField: "note_count", Op: maniflex.AggCount,
				Child: "cbNote", On: "cb_article_i",
			})
		},
	})

	aid := srv.MustID(srv.POST("/cb_authors", map[string]any{"name": "Ada"}))
	art := srv.MustID(srv.POST("/cb_articles", map[string]any{"title": "A", "cb_author_i": aid}))
	note := srv.MustID(srv.POST("/cb_notes", map[string]any{"body": "n", "cb_article_i": art}))

	srv.DELETE("/cb_authors/" + aid).AssertStatus(http.StatusNoContent)

	if got := srv.GET("/cb_articles/" + art).Status; got != http.StatusNotFound {
		t.Errorf("article survived the two-hop cascade: %d", got)
	}
	if got := srv.GET("/cb_notes/" + note).Status; got != http.StatusNotFound {
		t.Errorf("note survived the two-hop cascade: %d", got)
	}
}
