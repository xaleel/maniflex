package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit PIPE-9 — a rollup keyed on a nullable foreign key.
//
// affectedParents reads the pre-write child row through the accessor, whose
// recordToMap stores each struct field as it stands. A nullable FK — the shape
// onDelete:setNull requires — therefore arrives as a *string, and addParentID's
// fmt.Sprint rendered it as a pointer address; a NULL one rendered as "<nil>",
// which is non-empty and so entered the set as a parent id of its own. Either
// way recompute ran against an id no row has and the write failed, so every
// PATCH and DELETE of such a child answered 500 ROLLUP_ERROR — an orphan child
// could not be modified or deleted at all. Creates were unaffected, because
// there the value comes from ctx.ParsedBody as a JSON string.
//
//	go test ./tests/e2e/... -run TestRollupNullableFK

type RollupOrg struct {
	maniflex.BaseModel
	Name     string `json:"name"      db:"name"`
	DocCount int    `json:"doc_count" db:"doc_count"`
}

type RollupDoc struct {
	maniflex.BaseModel
	Title       string    `json:"title"         db:"title"`
	RollupOrgID *string   `json:"rollup_org_id" db:"rollup_org_id" mfx:"relation:RollupOrg;onDelete:setNull"`
	RollupOrg   RollupOrg `json:"-"`
}

func rollupNullableSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{RollupOrg{}, RollupDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
				maniflex.ForModel("RollupDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "RollupOrg", ParentField: "doc_count", Op: maniflex.AggCount,
				Child: "RollupDoc", On: "rollup_org_id",
			})
		},
	})
}

// docCount normalises the numeric type, which differs by lane (SQLite hands back
// the struct's int, Postgres an int64).
func docCount(t *testing.T, srv *testutil.Server, orgID string) int64 {
	t.Helper()
	var out int64
	srv.GET("/rollup_orgs/" + orgID).AssertJSON(func(body map[string]any) {
		data, _ := body["data"].(map[string]any)
		switch v := data["doc_count"].(type) {
		case float64:
			out = int64(v)
		case int64:
			out = v
		case int:
			out = int64(v)
		default:
			t.Fatalf("doc_count has unexpected type %T (%v)", v, v)
		}
	})
	return out
}

// TestRollupNullableFK_ReparentAndDeleteRecompute is the bug: both writes used
// to answer 500, leaving every total stale.
func TestRollupNullableFK_ReparentAndDeleteRecompute(t *testing.T) {
	srv := rollupNullableSrv(t)

	orgA := srv.MustID(srv.POST("/rollup_orgs", map[string]any{"name": "A"}))
	orgB := srv.MustID(srv.POST("/rollup_orgs", map[string]any{"name": "B"}))

	// The create path always worked — the value comes from the body as a string.
	docID := srv.MustID(srv.POST("/rollup_docs", map[string]any{
		"title": "d1", "rollup_org_id": orgA}))
	if got := docCount(t, srv, orgA); got != 1 {
		t.Fatalf("after create: org A count = %d, want 1", got)
	}

	// Re-parent: both the old parent and the new one must be recomputed.
	srv.PATCH("/rollup_docs/"+docID, map[string]any{"rollup_org_id": orgB}).
		AssertStatus(http.StatusOK)
	if got := docCount(t, srv, orgA); got != 0 {
		t.Errorf("after re-parent: org A count = %d, want 0", got)
	}
	if got := docCount(t, srv, orgB); got != 1 {
		t.Errorf("after re-parent: org B count = %d, want 1", got)
	}

	srv.DELETE("/rollup_docs/" + docID).AssertStatus(http.StatusNoContent)
	if got := docCount(t, srv, orgB); got != 0 {
		t.Errorf("after delete: org B count = %d, want 0", got)
	}
}

// TestRollupNullableFK_NullKeyChildStaysWritable is the nil branch. A typed nil
// inside an any is not == nil, so the old guard did not fire and "<nil>" was
// added to the set as a parent id — which made a child pointing at no parent
// impossible to update or delete.
func TestRollupNullableFK_NullKeyChildStaysWritable(t *testing.T) {
	srv := rollupNullableSrv(t)

	orgA := srv.MustID(srv.POST("/rollup_orgs", map[string]any{"name": "A"}))
	docID := srv.MustID(srv.POST("/rollup_docs", map[string]any{"title": "orphan"}))

	srv.PATCH("/rollup_docs/"+docID, map[string]any{"title": "orphan2"}).
		AssertStatus(http.StatusOK)
	srv.DELETE("/rollup_docs/" + docID).AssertStatus(http.StatusNoContent)

	// A child with no parent must not have disturbed anyone's total.
	if got := docCount(t, srv, orgA); got != 0 {
		t.Errorf("org A count = %d after an unrelated orphan child, want 0", got)
	}
}
