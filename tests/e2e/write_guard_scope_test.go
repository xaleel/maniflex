package e2e

// Audit STEP-1 / STEP-2 / PIPE-1 — three pre-write guards read the target row by
// id with no forced filters, and answered before enforceWriteScope ran. Their
// 422 / 412 / 404 told one tenant whether another tenant's row existed and what
// state it had reached, which is exactly the distinction enforceWriteScope
// documents itself as removing: "indistinguishable from a genuinely absent
// record, on purpose".
//
// lock_when leaked existence and state; If-Match leaked existence and took a FOR
// UPDATE lock on the foreign row for the rest of the transaction; lock_scope let
// a create probe and briefly lock any row of the referenced model by id.
//
// Everything a caller cannot see must answer 404 and nothing else — so these
// tests compare the cross-scope answers against the answer for an id that does
// not exist at all, rather than against a hard-coded status.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const absentID = "00000000-0000-0000-0000-000000000000"

// ScopedInvoice carries a lock_when rule and the column a forced filter scopes
// by. testutil.LockedInvoice has the rule but no scope column.
type ScopedInvoice struct {
	maniflex.BaseModel
	Number string  `json:"number" db:"number" mfx:"required,filterable"`
	Status string  `json:"status" db:"status" mfx:"required,enum:draft|posted,lock_when:status=posted"`
	OrgID  *string `json:"org_id" db:"org_id" mfx:"filterable"`
}

// ScopedAsset is the If-Match half — OptimisticLock plus the same scope column.
type ScopedAsset struct {
	maniflex.BaseModel
	Name  string  `json:"name"   db:"name"   mfx:"required"`
	OrgID *string `json:"org_id" db:"org_id" mfx:"filterable"`
}

// ScopedBatch is a lock_scope target that carries the scope column, and
// ScopedDraw is the child whose create locks it.
type ScopedBatch struct {
	maniflex.BaseModel
	Label string  `json:"label"  db:"label"  mfx:"required"`
	OrgID *string `json:"org_id" db:"org_id" mfx:"filterable"`
}

type ScopedDraw struct {
	maniflex.BaseModel
	BatchID string  `json:"batch_id" db:"batch_id" mfx:"required,lock_scope:ScopedBatch"`
	OrgID   *string `json:"org_id"   db:"org_id"   mfx:"filterable"`
}

func orgScope() maniflex.MiddlewareFunc {
	return dbmw.ForceFilter("org_id", func(ctx *maniflex.ServerContext) any {
		if org := ctx.Request.Header.Get("X-Org"); org != "" {
			return org
		}
		return nil
	})
}

// guardSrv registers the scope the conventional way — on the DB step, without
// ProvidesScope. That is the shape that leaked: the Validate step runs before
// this middleware, so the lock_when guard there has no scope to read through.
func guardSrv(t *testing.T, hoist bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{ScopedInvoice{}, ScopedBatch{}, ScopedDraw{}},
		Middleware: func(s *maniflex.Server) {
			for _, name := range []string{"ScopedInvoice", "ScopedBatch", "ScopedDraw"} {
				opts := []maniflex.MiddlewareOption{maniflex.ForModel(name)}
				if hoist {
					opts = append(opts, maniflex.ProvidesScope())
				}
				s.Pipeline.DB.Register(orgScope(), opts...)
			}
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("ScopedDraw"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})
}

var (
	asA = map[string]string{"X-Org": "tenant-a"}
	asB = map[string]string{"X-Org": "tenant-b"}
)

// postedAndDraft gives A one locked invoice and one that is not.
func postedAndDraft(t *testing.T, srv *testutil.Server) (posted, draft string) {
	t.Helper()
	posted = srv.MustID(srv.POST("/scoped_invoices",
		map[string]any{"number": "A-1", "status": "posted", "org_id": "tenant-a"}, asA))
	draft = srv.MustID(srv.POST("/scoped_invoices",
		map[string]any{"number": "A-2", "status": "draft", "org_id": "tenant-a"}, asA))
	return posted, draft
}

func TestWriteGuards_LockWhenDoesNotLeakStateAcrossScope(t *testing.T) {
	t.Parallel()
	srv := guardSrv(t, false)
	posted, draft := postedAndDraft(t, srv)

	// The reference answer: what B gets for an id that is not there at all.
	want := srv.PATCH("/scoped_invoices/"+absentID, map[string]any{"number": "X"}, asB)
	if want.Status != http.StatusNotFound {
		t.Fatalf("an absent id answered %d, not 404 — the comparison below is meaningless", want.Status)
	}

	for _, tc := range []struct{ label, id string }{
		{"A's locked invoice", posted},
		{"A's unlocked invoice", draft},
	} {
		got := srv.PATCH("/scoped_invoices/"+tc.id, map[string]any{"number": "X"}, asB)
		if got.Status != want.Status || got.ErrorCode() != want.ErrorCode() {
			t.Errorf("PATCH %s → %d %s; want %d %s, the same answer an absent id gets — "+
				"anything else tells B the row exists and what state it is in",
				tc.label, got.Status, got.ErrorCode(), want.Status, want.ErrorCode())
		}
		gotDel := srv.DELETE("/scoped_invoices/"+tc.id, asB)
		if gotDel.Status != http.StatusNotFound {
			t.Errorf("DELETE %s → %d %s; want 404", tc.label, gotDel.Status, gotDel.ErrorCode())
		}
	}
}

// The guard must still do its job for the tenant who owns the row.
func TestWriteGuards_LockWhenStillRefusesInScope(t *testing.T) {
	t.Parallel()
	srv := guardSrv(t, false)
	posted, draft := postedAndDraft(t, srv)

	locked := srv.PATCH("/scoped_invoices/"+posted, map[string]any{"number": "A-1b"}, asA)
	if locked.Status != http.StatusUnprocessableEntity || locked.ErrorCode() != "RECORD_LOCKED" {
		t.Errorf("A patching its own posted invoice → %d %s, want 422 RECORD_LOCKED",
			locked.Status, locked.ErrorCode())
	}
	if del := srv.DELETE("/scoped_invoices/"+posted, asA); del.Status != http.StatusUnprocessableEntity {
		t.Errorf("A deleting its own posted invoice → %d, want 422", del.Status)
	}
	// An unlocked row of A's still updates.
	srv.PATCH("/scoped_invoices/"+draft, map[string]any{"number": "A-2b"}, asA).
		AssertStatus(http.StatusOK)
}

// A hoisted scope runs before Validate, so the guard keeps its early abort
// there. Both the refusal and the non-disclosure have to hold in that shape too.
func TestWriteGuards_LockWhenWithHoistedScope(t *testing.T) {
	t.Parallel()
	srv := guardSrv(t, true)
	posted, _ := postedAndDraft(t, srv)

	if got := srv.PATCH("/scoped_invoices/"+posted, map[string]any{"number": "X"}, asB); got.Status != http.StatusNotFound {
		t.Errorf("cross-scope PATCH with a hoisted scope → %d %s, want 404",
			got.Status, got.ErrorCode())
	}
	if got := srv.PATCH("/scoped_invoices/"+posted, map[string]any{"number": "X"}, asA); got.ErrorCode() != "RECORD_LOCKED" {
		t.Errorf("in-scope PATCH of a locked row → %d %s, want 422 RECORD_LOCKED",
			got.Status, got.ErrorCode())
	}
}

func assetSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{ScopedAsset{}, maniflex.ModelConfig{OptimisticLock: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(orgScope(), maniflex.ForModel("ScopedAsset"))
			// The ETag a caller sends back as If-Match comes from response.Cache;
			// a plain read emits none.
			s.Pipeline.Response.Register(
				response.Cache(response.CacheConfig{}),
				maniflex.ForModel("ScopedAsset"),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})
}

func TestWriteGuards_IfMatchDoesNotLeakExistenceAcrossScope(t *testing.T) {
	t.Parallel()
	srv := assetSrv(t)
	id := srv.MustID(srv.POST("/scoped_assets",
		map[string]any{"name": "A's asset", "org_id": "tenant-a"}, asA))

	stale := map[string]string{"X-Org": "tenant-b", "If-Match": `"not-the-etag"`}
	present := srv.PATCH("/scoped_assets/"+id, map[string]any{"name": "X"}, stale)
	absent := srv.PATCH("/scoped_assets/"+absentID, map[string]any{"name": "X"}, stale)

	if present.Status != absent.Status || present.ErrorCode() != absent.ErrorCode() {
		t.Errorf("If-Match on A's id → %d %s but on an absent id → %d %s; a 412 here "+
			"tells B the row exists, and takes a row lock on it",
			present.Status, present.ErrorCode(), absent.Status, absent.ErrorCode())
	}
	if present.Status != http.StatusNotFound {
		t.Errorf("cross-scope If-Match → %d, want 404", present.Status)
	}
}

// The precondition still has to work for the owner: stale fails, current wins.
func TestWriteGuards_IfMatchStillWorksInScope(t *testing.T) {
	t.Parallel()
	srv := assetSrv(t)
	id := srv.MustID(srv.POST("/scoped_assets",
		map[string]any{"name": "A's asset", "org_id": "tenant-a"}, asA))

	stale := map[string]string{"X-Org": "tenant-a", "If-Match": `"not-the-etag"`}
	if got := srv.PATCH("/scoped_assets/"+id, map[string]any{"name": "X"}, stale); got.Status != http.StatusPreconditionFailed {
		t.Errorf("A's own stale If-Match → %d %s, want 412", got.Status, got.ErrorCode())
	}

	etag := srv.GET("/scoped_assets/"+id, asA).Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the read; the rest of this test cannot run")
	}
	current := map[string]string{"X-Org": "tenant-a", "If-Match": etag}
	srv.PATCH("/scoped_assets/"+id, map[string]any{"name": "renamed"}, current).
		AssertStatus(http.StatusOK)
}

func TestWriteGuards_LockScopeDoesNotLeakAcrossScope(t *testing.T) {
	t.Parallel()
	srv := guardSrv(t, false)

	batch := srv.MustID(srv.POST("/scoped_batches",
		map[string]any{"label": "A's batch", "org_id": "tenant-a"}, asA))

	// B creating a draw against A's batch must look exactly like B creating one
	// against a batch that does not exist — otherwise the create is an oracle,
	// and a successful one holds a lock on A's row.
	onAs := srv.POST("/scoped_draws", map[string]any{"batch_id": batch}, asB)
	onAbsent := srv.POST("/scoped_draws", map[string]any{"batch_id": absentID}, asB)

	if onAs.Status != onAbsent.Status || onAs.ErrorCode() != onAbsent.ErrorCode() {
		t.Errorf("create against A's batch → %d %s but against an absent id → %d %s",
			onAs.Status, onAs.ErrorCode(), onAbsent.Status, onAbsent.ErrorCode())
	}
	if onAs.Status != http.StatusNotFound {
		t.Errorf("create against another tenant's lock_scope target → %d, want 404", onAs.Status)
	}
}

// And the ordinary case still works: A's own batch takes the lock and inserts.
func TestWriteGuards_LockScopeStillWorksInScope(t *testing.T) {
	t.Parallel()
	srv := guardSrv(t, false)

	batch := srv.MustID(srv.POST("/scoped_batches",
		map[string]any{"label": "A's batch", "org_id": "tenant-a"}, asA))
	srv.POST("/scoped_draws", map[string]any{"batch_id": batch, "org_id": "tenant-a"}, asA).
		AssertStatus(http.StatusCreated)
}
