package e2e

import (
	"net/http"
	"testing"

	maniflex "github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// 5.16 — onDelete actions enforced at the maniflex layer. Each explicit relation
// carries an onDelete action on the child's FK; deleting the parent applies it.
//
// Soft-vs-hard is proved through a unique column: a soft-deleted row still holds
// its unique value (reuse → 409), a hard-deleted one frees it (reuse → 201).

type csAuthor struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// csPost hard-deletes and cascades from its author.
type csPost struct {
	maniflex.BaseModel
	Slug     string   `json:"slug" mfx:"unique"`
	AuthorID string   `json:"author_id" mfx:"relation:CsAuthor;onDelete:cascade"`
	CsAuthor csAuthor `json:"-"`
}

// csComment cascades from its post — the second hop of the recursion test.
type csComment struct {
	maniflex.BaseModel
	Body   string `json:"body"`
	PostID string `json:"post_id" mfx:"relation:CsPost;onDelete:cascade"`
	CsPost csPost `json:"-"`
}

type csTeam struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// csMember soft-deletes and cascades from its team — a soft-delete cascade.
type csMember struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Email  string `json:"email" mfx:"unique"`
	TeamID string `json:"team_id" mfx:"relation:CsTeam;onDelete:cascade"`
	CsTeam csTeam `json:"-"`
}

type csOrg struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// csDoc nulls its org FK when the org is deleted; the FK is a pointer so it is nullable.
type csDoc struct {
	maniflex.BaseModel
	Title string  `json:"title"`
	OrgID *string `json:"org_id" mfx:"relation:CsOrg;onDelete:setNull"`
	CsOrg csOrg   `json:"-"`
}

type csVendor struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// csBill restricts its vendor's deletion; csVendorNote cascades from it.
type csBill struct {
	maniflex.BaseModel
	Amount   int      `json:"amount"`
	VendorID string   `json:"vendor_id" mfx:"relation:CsVendor;onDelete:restrict"`
	CsVendor csVendor `json:"-"`
}

type csVendorNote struct {
	maniflex.BaseModel
	Note     string   `json:"note"`
	VendorID string   `json:"vendor_id" mfx:"relation:CsVendor;onDelete:cascade"`
	CsVendor csVendor `json:"-"`
}

func cascadeTestModels() []any {
	return []any{
		csAuthor{}, csPost{}, csComment{},
		csTeam{}, csMember{},
		csOrg{}, csDoc{},
		csVendor{}, csBill{}, csVendorNote{},
	}
}

func cascadeServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: cascadeTestModels()})
}

func TestCascadeDelete(t *testing.T) {
	t.Parallel()

	t.Run("cascade_hard_deletes_children", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		author := srv.POST("/cs_authors", map[string]any{"name": "A"}).ID()
		post := srv.POST("/cs_posts", map[string]any{"slug": "s1", "author_id": author})
		post.AssertStatus(http.StatusCreated)
		postID := post.ID()

		srv.DELETE("/cs_authors/" + author).AssertStatus(http.StatusNoContent)
		srv.GET("/cs_posts/" + postID).AssertStatus(http.StatusNotFound)

		// Hard delete: the slug is freed, so a new post can reuse it.
		author2 := srv.POST("/cs_authors", map[string]any{"name": "A2"}).ID()
		srv.POST("/cs_posts", map[string]any{"slug": "s1", "author_id": author2}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("cascade_soft_deletes_children_identically", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		team := srv.POST("/cs_teams", map[string]any{"name": "T"}).ID()
		member := srv.POST("/cs_members", map[string]any{"email": "a@x.com", "team_id": team})
		member.AssertStatus(http.StatusCreated)
		memberID := member.ID()

		srv.DELETE("/cs_teams/" + team).AssertStatus(http.StatusNoContent)

		// The member is hidden from the API…
		srv.GET("/cs_members/" + memberID).AssertStatus(http.StatusNotFound)
		// …but soft-deleted, not removed: its unique email is still reserved, so a
		// new member cannot take it (a hard delete would have freed it).
		team2 := srv.POST("/cs_teams", map[string]any{"name": "T2"}).ID()
		srv.POST("/cs_members", map[string]any{"email": "a@x.com", "team_id": team2}).
			AssertStatus(http.StatusConflict)
	})

	t.Run("cascade_recurses_to_grandchildren", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		author := srv.POST("/cs_authors", map[string]any{"name": "A"}).ID()
		post := srv.POST("/cs_posts", map[string]any{"slug": "r1", "author_id": author}).ID()
		comment := srv.POST("/cs_comments", map[string]any{"body": "c", "post_id": post})
		comment.AssertStatus(http.StatusCreated)
		commentID := comment.ID()

		srv.DELETE("/cs_authors/" + author).AssertStatus(http.StatusNoContent)
		srv.GET("/cs_posts/" + post).AssertStatus(http.StatusNotFound)
		srv.GET("/cs_comments/" + commentID).AssertStatus(http.StatusNotFound)
	})

	t.Run("setNull_nulls_child_fk", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		org := srv.POST("/cs_orgs", map[string]any{"name": "O"}).ID()
		doc := srv.POST("/cs_docs", map[string]any{"title": "D", "org_id": org})
		doc.AssertStatus(http.StatusCreated)
		docID := doc.ID()

		srv.DELETE("/cs_orgs/" + org).AssertStatus(http.StatusNoContent)

		got := srv.GET("/cs_docs/" + docID)
		got.AssertStatus(http.StatusOK)
		if v := got.Data()["org_id"]; v != nil {
			t.Errorf("org_id = %v, want null after setNull", v)
		}
	})

	t.Run("restrict_blocks_delete_with_children", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		vendor := srv.POST("/cs_vendors", map[string]any{"name": "V"}).ID()
		srv.POST("/cs_bills", map[string]any{"amount": 10, "vendor_id": vendor}).
			AssertStatus(http.StatusCreated)

		srv.DELETE("/cs_vendors/" + vendor).AssertStatus(http.StatusConflict)
		// The vendor survives the refused delete.
		srv.GET("/cs_vendors/" + vendor).AssertStatus(http.StatusOK)
	})

	t.Run("restrict_allows_delete_with_no_children", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		vendor := srv.POST("/cs_vendors", map[string]any{"name": "Empty"}).ID()
		srv.DELETE("/cs_vendors/" + vendor).AssertStatus(http.StatusNoContent)
	})

	t.Run("db_fk_enforces_referential_integrity", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		// csPost→csAuthor is hard/hard, so Phase 3 emits a real FK constraint: the
		// database refuses a post that names a non-existent author. Were the edge
		// only app-enforced (no constraint), this insert would succeed with 201.
		srv.POST("/cs_posts", map[string]any{"slug": "orphan", "author_id": "no-such-author"}).
			AssertStatus(http.StatusConflict)

		// A soft-delete edge (csMember→csTeam) is app-enforced, so no FK is emitted
		// and the same bad-parent insert is not caught by the database.
		srv.POST("/cs_members", map[string]any{"email": "b@x.com", "team_id": "no-such-team"}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("restrict_rolls_back_a_sibling_cascade", func(t *testing.T) {
		t.Parallel()
		srv := cascadeServer(t)

		vendor := srv.POST("/cs_vendors", map[string]any{"name": "V"}).ID()
		srv.POST("/cs_bills", map[string]any{"amount": 5, "vendor_id": vendor}).
			AssertStatus(http.StatusCreated)
		note := srv.POST("/cs_vendor_notes", map[string]any{"note": "n", "vendor_id": vendor})
		note.AssertStatus(http.StatusCreated)
		noteID := note.ID()

		// The bill's restrict aborts the delete; the note's cascade must roll back
		// with it, so the note still exists.
		srv.DELETE("/cs_vendors/" + vendor).AssertStatus(http.StatusConflict)
		srv.GET("/cs_vendor_notes/" + noteID).AssertStatus(http.StatusOK)
	})
}

// Audit NEW-4. A Postgres deployment that puts the same models in more than one
// schema — schema-per-tenant, which postgres.SessionConfig.SchemaName exists to
// support — got its foreign keys in whichever schema migrated first and silently
// none thereafter. The adapter's constraint-existence probe queried
// information_schema.table_constraints without a table_schema predicate, and
// information_schema spans every schema the role can see; constraint names are
// derived from table and column, so two schemas holding one model collide by
// construction. The probe answered "already present" and the ALTER TABLE that
// would have created the constraint here was skipped.
//
// Two servers is the whole reproduction: the e2e Postgres lane gives each its
// own schema, so the second is the one that used to come out unprotected. This
// is also why TestCascadeDelete's other subtests failed in varying combinations
// on Postgres and each passed when run alone — only the first to migrate had a
// foreign key.
func TestCascadeDelete_ForeignKeysReachEverySchema(t *testing.T) {
	t.Parallel()
	skipUnlessPostgres(t)

	first := cascadeServer(t)
	second := cascadeServer(t)

	// Establishes the baseline: the first schema to migrate was never the broken
	// one, so a failure here means something other than NEW-4 is wrong.
	first.POST("/cs_posts", map[string]any{"slug": "orphan-1", "author_id": "no-such-author"}).
		AssertStatus(http.StatusConflict)

	// The regression: same models, a second schema, same guarantee.
	second.POST("/cs_posts", map[string]any{"slug": "orphan-2", "author_id": "no-such-author"}).
		AssertStatus(http.StatusConflict)

	// The onDelete actions ride on that same constraint, so confirm the second
	// schema cascades rather than merely rejecting orphans.
	author := second.POST("/cs_authors", map[string]any{"name": "A"}).ID()
	post := second.POST("/cs_posts", map[string]any{"slug": "s1", "author_id": author})
	post.AssertStatus(http.StatusCreated)
	second.DELETE("/cs_authors/" + author).AssertStatus(http.StatusNoContent)
	second.GET("/cs_posts/" + post.ID()).AssertStatus(http.StatusNotFound)
}
