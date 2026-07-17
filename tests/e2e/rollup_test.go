package e2e

// R12 — maniflex.Rollup: a denormalised aggregate column on a parent, kept in
// step with its children by the framework. Order.PaidAmount = SUM(payment.amount),
// recomputed on every child write inside the request's transaction.

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// rollupServer registers Order+OrderPayment with a SUM and a COUNT rollup, and
// WithTransaction on the child's writes (required for a rollup).
func rollupServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.Order{}, testutil.OrderPayment{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("OrderPayment"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
			)
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "Order", ParentField: "paid_amount", Op: maniflex.AggSum,
				Child: "OrderPayment", ChildField: "amount", On: "order_id",
			})
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "Order", ParentField: "payment_count", Op: maniflex.AggCount,
				Child: "OrderPayment", On: "order_id",
			})
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "Order", ParentField: "top_payment", Op: maniflex.AggMax,
				Child: "OrderPayment", ChildField: "amount", On: "order_id",
			})
		},
	})
}

func newOrder(t *testing.T, srv *testutil.Server, ref string) string {
	t.Helper()
	resp := srv.POST("/orders", map[string]any{"reference": ref})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

func addPayment(t *testing.T, srv *testutil.Server, orderID string, amount int) string {
	t.Helper()
	resp := srv.POST("/order_payments", map[string]any{"order_id": orderID, "amount": amount})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// orderField reads one field off an order.
func orderField(t *testing.T, srv *testutil.Server, orderID, field string) any {
	t.Helper()
	resp := srv.GET("/orders/"+orderID, nil)
	resp.AssertStatus(http.StatusOK)
	return resp.Data()[field]
}

func intField(t *testing.T, srv *testutil.Server, orderID, field string) int {
	t.Helper()
	v := orderField(t, srv, orderID, field)
	f, ok := v.(float64) // JSON numbers
	if !ok {
		t.Fatalf("%s is not numeric: %#v", field, v)
	}
	return int(f)
}

// ── maintenance on child writes ─────────────────────────────────────────────

func TestRollup_SumMaintainedOnCreate(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-1")

	if got := intField(t, srv, o, "paid_amount"); got != 0 {
		t.Fatalf("paid_amount before any payment: got %d, want 0", got)
	}
	addPayment(t, srv, o, 30)
	addPayment(t, srv, o, 12)

	if got := intField(t, srv, o, "paid_amount"); got != 42 {
		t.Errorf("paid_amount after 30+12: got %d, want 42", got)
	}
	if got := intField(t, srv, o, "payment_count"); got != 2 {
		t.Errorf("payment_count: got %d, want 2", got)
	}
}

func TestRollup_MaintainedOnUpdate(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-2")
	p := addPayment(t, srv, o, 30)
	addPayment(t, srv, o, 10)

	// Raise the first payment 30 -> 100.
	srv.PATCH("/order_payments/"+p, map[string]any{"amount": 100}, nil).
		AssertStatus(http.StatusOK)

	if got := intField(t, srv, o, "paid_amount"); got != 110 {
		t.Errorf("paid_amount after update to 100+10: got %d, want 110", got)
	}
}

func TestRollup_MaintainedOnDelete(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-3")
	p := addPayment(t, srv, o, 30)
	addPayment(t, srv, o, 12)

	srv.DELETE("/order_payments/"+p, nil).AssertStatus(http.StatusNoContent)

	if got := intField(t, srv, o, "paid_amount"); got != 12 {
		t.Errorf("paid_amount after deleting the 30 payment: got %d, want 12", got)
	}
	if got := intField(t, srv, o, "payment_count"); got != 1 {
		t.Errorf("payment_count after delete: got %d, want 1", got)
	}
}

// A soft-deleted child must drop out of the rollup — the recompute excludes it.
func TestRollup_ExcludesSoftDeletedChildren(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-4")
	p := addPayment(t, srv, o, 50)
	addPayment(t, srv, o, 5)

	// OrderPayment is WithDeletedAt, so DELETE soft-deletes.
	srv.DELETE("/order_payments/"+p, nil).AssertStatus(http.StatusNoContent)

	if got := intField(t, srv, o, "paid_amount"); got != 5 {
		t.Errorf("paid_amount after soft-deleting the 50 payment: got %d, want 5 "+
			"(a soft-deleted child must not count)", got)
	}
}

// ── re-parenting: the FK moves, both parents recompute ──────────────────────

func TestRollup_ReparentingRecomputesBothParents(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	a := newOrder(t, srv, "O-A")
	b := newOrder(t, srv, "O-B")
	p := addPayment(t, srv, a, 40)

	if got := intField(t, srv, a, "paid_amount"); got != 40 {
		t.Fatalf("order A before move: got %d, want 40", got)
	}

	// Move the payment from A to B.
	srv.PATCH("/order_payments/"+p, map[string]any{"order_id": b}, nil).
		AssertStatus(http.StatusOK)

	if got := intField(t, srv, a, "paid_amount"); got != 0 {
		t.Errorf("order A after the payment moved away: got %d, want 0", got)
	}
	if got := intField(t, srv, b, "paid_amount"); got != 40 {
		t.Errorf("order B after the payment moved in: got %d, want 40", got)
	}
}

// ── empty-set values ────────────────────────────────────────────────────────

// Sum/count of no children is 0; max of none is NULL, not 0.
func TestRollup_EmptySetValues(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-5")
	p := addPayment(t, srv, o, 70)
	srv.DELETE("/order_payments/"+p, nil).AssertStatus(http.StatusNoContent)

	if got := intField(t, srv, o, "paid_amount"); got != 0 {
		t.Errorf("sum of no children: got %d, want 0", got)
	}
	if got := intField(t, srv, o, "payment_count"); got != 0 {
		t.Errorf("count of no children: got %d, want 0", got)
	}
	if got := orderField(t, srv, o, "top_payment"); got != nil {
		t.Errorf("max of no children: got %#v, want null", got)
	}
}

func TestRollup_MaxIsMaintained(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-6")
	addPayment(t, srv, o, 10)
	addPayment(t, srv, o, 90)
	addPayment(t, srv, o, 40)

	if got := intField(t, srv, o, "top_payment"); got != 90 {
		t.Errorf("top_payment: got %d, want 90", got)
	}
}

// ── isolation: a child of one parent never touches another ──────────────────

func TestRollup_ParentsAreIsolated(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	a := newOrder(t, srv, "O-iso-A")
	b := newOrder(t, srv, "O-iso-B")
	addPayment(t, srv, a, 15)
	addPayment(t, srv, b, 25)

	if got := intField(t, srv, a, "paid_amount"); got != 15 {
		t.Errorf("order A: got %d, want 15", got)
	}
	if got := intField(t, srv, b, "paid_amount"); got != 25 {
		t.Errorf("order B: got %d, want 25", got)
	}
}

// ── no transaction is refused ───────────────────────────────────────────────

func TestRollup_NoTransactionIsRefused(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.Order{}, testutil.OrderPayment{}},
		Middleware: func(s *maniflex.Server) {
			// Deliberately no WithTransaction on OrderPayment writes.
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "Order", ParentField: "paid_amount", Op: maniflex.AggSum,
				Child: "OrderPayment", ChildField: "amount", On: "order_id",
			})
		},
	})
	o := newOrder(t, srv, "O-notx")

	resp := srv.POST("/order_payments", map[string]any{"order_id": o, "amount": 10})
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "ROLLUP_NO_TX" {
		t.Errorf("error code: got %q, want ROLLUP_NO_TX", code)
	}
}

// ── backfill ────────────────────────────────────────────────────────────────

// BackfillRollups reconciles a column that drifted (here, set directly on the
// parent while children already exist).
func TestRollup_Backfill(t *testing.T) {
	t.Parallel()
	srv := rollupServer(t)
	o := newOrder(t, srv, "O-bf")
	addPayment(t, srv, o, 20)
	addPayment(t, srv, o, 22)

	// Corrupt the maintained column out of band.
	srv.PATCH("/orders/"+o, map[string]any{"paid_amount": 999}, nil).AssertStatus(http.StatusOK)
	if got := intField(t, srv, o, "paid_amount"); got != 999 {
		t.Fatalf("setup: paid_amount should be the corrupted 999, got %d", got)
	}

	if err := srv.ManiflexServer().BackfillRollups(context.Background()); err != nil {
		t.Fatalf("BackfillRollups: %v", err)
	}

	if got := intField(t, srv, o, "paid_amount"); got != 42 {
		t.Errorf("paid_amount after backfill: got %d, want 42", got)
	}
	if got := intField(t, srv, o, "payment_count"); got != 2 {
		t.Errorf("payment_count after backfill: got %d, want 2", got)
	}
}
