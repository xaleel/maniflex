package e2e

// 5.1 — mfx:"lock_when" record immutability. A record whose state matches any
// lock_when condition is locked: updates and deletes return 422 RECORD_LOCKED.
// Creates are not affected (no prior state to compare).

import (
	"net/http"
	"testing"

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
