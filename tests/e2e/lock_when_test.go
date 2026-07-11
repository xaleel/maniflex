package e2e

// 5.1 — mfx:"lock_when" record immutability. A record whose state matches any
// lock_when condition is locked: updates and deletes return 422 RECORD_LOCKED.
// Creates are not affected (no prior state to compare).

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func lockedInvoiceServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.LockedInvoice{}},
	})
}

func TestLockWhen_DraftIsEditable(t *testing.T) {
	t.Parallel()
	srv := lockedInvoiceServer(t)

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-001",
		"status": "draft",
		"amount": 100,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	// Draft → editable.
	srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 200}).AssertStatus(http.StatusOK)
	// And deletable.
	srv.DELETE("/locked_invoices/" + id).AssertStatus(http.StatusNoContent)
}

func TestLockWhen_PostedBlocksUpdate(t *testing.T) {
	t.Parallel()
	srv := lockedInvoiceServer(t)

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-002",
		"status": "posted", // locked from creation — caller's choice
		"amount": 50,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	resp := srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 999})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "RECORD_LOCKED" {
		t.Errorf("error code: got %q, want RECORD_LOCKED", code)
	}
}

func TestLockWhen_PostedBlocksDelete(t *testing.T) {
	t.Parallel()
	srv := lockedInvoiceServer(t)

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-003",
		"status": "posted",
		"amount": 25,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	resp := srv.DELETE("/locked_invoices/" + id)
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "RECORD_LOCKED" {
		t.Errorf("error code: got %q, want RECORD_LOCKED", code)
	}
}

func TestLockWhen_TransitionDraftToPostedThenLocks(t *testing.T) {
	t.Parallel()
	srv := lockedInvoiceServer(t)

	// Start in draft.
	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-004",
		"status": "draft",
		"amount": 10,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	// Transition to posted — this update itself is allowed (the existing
	// record is still draft when the check runs).
	srv.PATCH("/locked_invoices/"+id, map[string]any{"status": "posted"}).AssertStatus(http.StatusOK)

	// Now the record is locked.
	resp := srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 99})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "RECORD_LOCKED" {
		t.Errorf("error code: got %q, want RECORD_LOCKED", code)
	}
}

func TestLockWhen_VoidAlsoLocks(t *testing.T) {
	// Multiple lock_when directives on the same field: any match locks.
	t.Parallel()
	srv := lockedInvoiceServer(t)

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-005",
		"status": "void",
		"amount": 5,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	resp := srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 6})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "RECORD_LOCKED" {
		t.Errorf("error code: got %q, want RECORD_LOCKED", code)
	}
}

func TestLockWhen_MissingRecordReturns404NotLockError(t *testing.T) {
	// A non-existent record should still produce 404, not RECORD_LOCKED —
	// checkRecordLocked must defer to the normal flow when FindByID fails.
	t.Parallel()
	srv := lockedInvoiceServer(t)

	resp := srv.PATCH("/locked_invoices/nonexistent-id", map[string]any{"amount": 1})
	resp.AssertStatus(http.StatusNotFound)
}

// ── The guard fails closed ───────────────────────────────────────────────────

// flakyReadAdapter fails the guard's read on demand — a transient DB error
// (replica hiccup, dropped connection), not a missing record.
type flakyReadAdapter struct {
	maniflex.DBAdapter
	failReads atomic.Bool
}

func (a *flakyReadAdapter) FindByID(ctx context.Context, model *maniflex.ModelMeta, id string,
	q *maniflex.QueryParams) (any, error) {
	if a.failReads.Load() {
		return nil, errors.New("connection reset by peer")
	}
	return a.DBAdapter.FindByID(ctx, model, id, q)
}

// A lock_when guard that cannot read the record must fail the request, not wave
// it through. Any read error other than "no such record" used to be swallowed,
// so a brief DB hiccup during a PATCH or DELETE let a locked record be modified
// (BUG-3).
func TestLockWhen_GuardReadFailureBlocksWrite(t *testing.T) {
	t.Parallel()

	adapter := &flakyReadAdapter{}
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.LockedInvoice{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			adapter.DBAdapter = inner
			return adapter, nil
		},
	})

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-006",
		"status": "posted", // locked
		"amount": 25,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	adapter.failReads.Store(true)

	resp := srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 999})
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "DB_ERROR" {
		t.Errorf("update error code: got %q, want DB_ERROR", code)
	}

	delResp := srv.DELETE("/locked_invoices/" + id)
	delResp.AssertStatus(http.StatusInternalServerError)
	if code := delResp.ErrorCode(); code != "DB_ERROR" {
		t.Errorf("delete error code: got %q, want DB_ERROR", code)
	}

	// The locked record is untouched: neither write got past the broken guard.
	adapter.failReads.Store(false)
	got := srv.GET("/locked_invoices/" + id).AssertStatus(http.StatusOK).Data()
	if got["amount"] != float64(25) {
		t.Errorf("amount = %v, want 25 — the update slipped past a failed lock check", got["amount"])
	}
	if got["status"] != "posted" {
		t.Errorf("status = %v, want posted", got["status"])
	}
}

// The guard reads through the request's transaction, so it sees the state the
// write is about to act on — including a lock the same request just applied and
// has not committed yet. Reading around the transaction sees a stale row and
// lets the write through (BUG-3).
func TestLockWhen_GuardSeesUncommittedStateInTransaction(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.LockedInvoice{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("LockedInvoice"),
				maniflex.ForOperation(maniflex.OpDelete),
			)
			// Runs inside that transaction, before the DB step: post the invoice,
			// which locks it. The delete's guard must observe this uncommitted write.
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					if _, err := ctx.RawExec(
						"UPDATE locked_invoices SET status = 'posted' WHERE id = ?",
						ctx.ResourceID); err != nil {
						return err
					}
					return next()
				},
				maniflex.ForModel("LockedInvoice"),
				maniflex.ForOperation(maniflex.OpDelete),
			)
		},
	})

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-007",
		"status": "draft", // committed state is unlocked …
		"amount": 40,
	})
	createResp.AssertStatus(http.StatusCreated)
	id := createResp.ID()

	// … but by the time the guard runs, this request has posted it.
	resp := srv.DELETE("/locked_invoices/" + id)
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "RECORD_LOCKED" {
		t.Errorf("error code: got %q, want RECORD_LOCKED", code)
	}

	// The 422 rolled the transaction back, so the invoice survives as a draft.
	got := srv.GET("/locked_invoices/" + id).AssertStatus(http.StatusOK).Data()
	if got["status"] != "draft" {
		t.Errorf("status = %v, want draft (the rejected request's write must roll back)", got["status"])
	}
}
