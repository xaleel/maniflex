package e2e

// 5.11 — mfx:"lock_scope:Model" auto-row-lock. Before a create, the DB step
// acquires a FOR UPDATE lock on the referenced row. Requires an active
// transaction (maniflex.WithTransaction(nil) on the Service step).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// lockScopeSrv returns a server with StockBalance and Dispense registered.
// WithTransaction is wired on the Service step for Dispense creates so the
// lock is held for the full insert.
func lockScopeSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.StockBalance{}, testutil.Dispense{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("Dispense"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})
}

func TestLockScope_CreateSucceedsWhenRefExists(t *testing.T) {
	t.Parallel()
	srv := lockScopeSrv(t)

	stockResp := srv.POST("/stock_balances", map[string]any{
		"name":     "Paracetamol 500mg",
		"quantity": 100,
	})
	stockResp.AssertStatus(http.StatusCreated)
	stockID := stockResp.ID()

	resp := srv.POST("/dispenses", map[string]any{
		"stock_id": stockID,
		"quantity": 5,
	})
	resp.AssertStatus(http.StatusCreated)
	m := resp.Data()
	if testutil.Field(t, m, "stock_id") != stockID {
		t.Errorf("dispense: got stock_id %v, want %s", m["stock_id"], stockID)
	}
}

func TestLockScope_CreateReturns404WhenRefMissing(t *testing.T) {
	t.Parallel()
	srv := lockScopeSrv(t)

	resp := srv.POST("/dispenses", map[string]any{
		"stock_id": "00000000-0000-0000-0000-000000000000",
		"quantity": 1,
	})
	resp.AssertStatus(http.StatusNotFound)
	if code := resp.ErrorCode(); code != "NOT_FOUND" {
		t.Errorf("error code: got %q, want NOT_FOUND", code)
	}
}

func TestLockScope_CreateFailsWithoutTransaction(t *testing.T) {
	t.Parallel()
	// Server without WithTransaction registered.
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.StockBalance{}, testutil.Dispense{}},
	})

	stockResp := srv.POST("/stock_balances", map[string]any{
		"name":     "Ibuprofen 400mg",
		"quantity": 50,
	})
	stockResp.AssertStatus(http.StatusCreated)
	stockID := stockResp.ID()

	resp := srv.POST("/dispenses", map[string]any{
		"stock_id": stockID,
		"quantity": 2,
	})
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "LOCK_SCOPE_NO_TX" {
		t.Errorf("error code: got %q, want LOCK_SCOPE_NO_TX", code)
	}
}

func TestLockScope_SkipsLockWhenFieldAbsent(t *testing.T) {
	// When the lock_scope field is absent (and not required), the lock step
	// is skipped. Here stock_id IS required so we verify the required
	// validation fires before the lock — the result is 422, not 500.
	t.Parallel()
	srv := lockScopeSrv(t)

	resp := srv.POST("/dispenses", map[string]any{
		"quantity": 3,
		// stock_id omitted
	})
	resp.AssertStatus(http.StatusUnprocessableEntity)
}

func TestLockScope_ValidatesBadModelAtStartup(t *testing.T) {
	// Registering a model whose lock_scope references a non-existent model
	// must panic at Handler() time (server startup).
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unknown lock_scope model, got none")
		}
	}()

	type BadDispense struct {
		maniflex.BaseModel
		RefID string `json:"ref_id" db:"ref_id" mfx:"lock_scope:NonExistentModel"`
	}

	srv := maniflex.New(maniflex.Config{})
	srv.MustRegister(BadDispense{})
	_ = srv.Handler() // must panic here
}
