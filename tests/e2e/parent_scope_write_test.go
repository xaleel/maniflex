package e2e

// Audit PIPE-2 — a create or update could point a foreign key at another
// tenant's parent. The child carries its own scope column, the write stamps the
// caller's own value onto it, and the row therefore looked correctly owned
// however wild the key was: nothing examined the key at all. enforceParentScope
// checked one only when the scope itself ran through it (db.ForceFilterVia).
//
// The parent is a row of someone else's, and the framework writes to it. A
// rollup recomputes the parent's denormalised column over every child naming it,
// so a planted child moved a total in a row its author could neither read nor
// reach — a silent cross-tenant write of business data. Refusing the write is
// the only durable answer: BackfillRollups aggregates by foreign key with no
// scope at all, so a row merely left out of the live recompute would be folded
// back in at the next reconcile.
//
// The other half of these tests is the part that must not break. A scope that
// refused every foreign key would be no better: a parent with no scope column is
// shared and stays writable, a null key is legal, and a key naming a model this
// app never registered has no parent table to check.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type psOrder struct {
	maniflex.BaseModel
	Reference  string `json:"reference"`
	PaidAmount int64  `json:"paid_amount" db:"paid_amount"`
	OrgID      string `json:"org_id" db:"org_id"`
}

// psCurrency is shared rather than partitioned — no org_id column, so no scope
// can be measured against it and every tenant's payments may name it. This is
// the same rule MS-9 applies to an included lookup model.
type psCurrency struct {
	maniflex.BaseModel
	Code string `json:"code"`
}

type psPayment struct {
	maniflex.BaseModel
	PsOrderID    string      `json:"ps_order_id"    db:"ps_order_id"    mfx:"relation"`
	PsOrder      *psOrder    `json:"ps_order,omitempty"`
	PsCurrencyID string      `json:"ps_currency_id" db:"ps_currency_id" mfx:"relation"`
	PsCurrency   *psCurrency `json:"ps_currency,omitempty"`
	// PsLedgerID is a convention FK (<Name>ID) to a model this app never
	// registers: the documented microservice case of storing a foreign id by
	// design. There is no parent table here, so there is nothing to check.
	PsLedgerID string `json:"ps_ledger_id" db:"ps_ledger_id"`
	Amount     int64  `json:"amount"`
	OrgID      string `json:"org_id" db:"org_id"`
}

func psSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{psOrder{}, psCurrency{}, psPayment{}},
		Middleware: func(s *maniflex.Server) {
			for _, m := range []string{"psOrder", "psPayment"} {
				s.Pipeline.DB.Register(orgScope(), maniflex.ForModel(m))
			}
			s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
				maniflex.ForModel("psPayment"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
			s.MustRegisterRollup(maniflex.Rollup{
				Parent: "psOrder", ParentField: "paid_amount", Op: maniflex.AggSum,
				Child: "psPayment", ChildField: "amount", On: "ps_order_id",
			})
		},
	})
}

func psOrderID(t *testing.T, srv *testutil.Server, ref, org string, as map[string]string) string {
	t.Helper()
	return srv.MustID(srv.POST("/ps_orders",
		map[string]any{"reference": ref, "org_id": org}, as))
}

// psPaid reads the parent's rollup column, which is the value the whole finding
// is about.
func psPaid(t *testing.T, srv *testutil.Server, orderID string, as map[string]string) float64 {
	t.Helper()
	v := srv.GET("/ps_orders/"+orderID, as).AssertStatus(http.StatusOK).Data()["paid_amount"]
	f, ok := v.(float64) // JSON numbers
	if !ok {
		t.Fatalf("paid_amount is not numeric: %#v", v)
	}
	return f
}

// ── the vector ───────────────────────────────────────────────────────────────

func TestParentScope_ForeignKeyToAnotherTenantIsRefused(t *testing.T) {
	t.Parallel()
	srv := psSrv(t)

	oid := psOrderID(t, srv, "A-1", "tenant-a", asA)
	srv.POST("/ps_payments", map[string]any{
		"ps_order_id": oid, "amount": 100, "org_id": "tenant-a"}, asA).
		AssertStatus(http.StatusCreated)
	if got := psPaid(t, srv, oid, asA); got != 100 {
		t.Fatalf("A's own rollup = %v, want 100 — the rest of this test proves nothing otherwise", got)
	}

	resp := srv.POST("/ps_payments", map[string]any{
		"ps_order_id": oid, "amount": 999000, "org_id": "tenant-b"}, asB)
	resp.AssertStatus(http.StatusNotFound)
	// The parent's own 404 and nothing more: a caller who cannot read that order
	// learns exactly what a read of it would have told them.
	if strings.Contains(string(resp.Body), "A-1") {
		t.Errorf("the refusal disclosed the order's contents: %s", resp.Body)
	}

	if got := psPaid(t, srv, oid, asA); got != 100 {
		t.Errorf("A's rollup moved to %v on B's write — PIPE-2 is open", got)
	}
	// And the row is gone, not merely uncounted: BackfillRollups aggregates by
	// foreign key with no scope, so a row left in the table would be folded into
	// A's total at the next reconcile.
	if rows := srv.GET("/ps_payments", asB).AssertStatus(http.StatusOK).DataList(); len(rows) != 0 {
		t.Errorf("B's payment was stored anyway: %#v", rows)
	}
}

// The update half. enforceWriteScope reads the row back through the scope, but
// that is the row as it stands — a PATCH rewriting the key passes a check of
// where the row used to be and then moves it somewhere else.
func TestParentScope_UpdateCannotRepointAKeyAtAnotherTenant(t *testing.T) {
	t.Parallel()
	srv := psSrv(t)

	aOrder := psOrderID(t, srv, "A-1", "tenant-a", asA)
	srv.POST("/ps_payments", map[string]any{
		"ps_order_id": aOrder, "amount": 100, "org_id": "tenant-a"}, asA).
		AssertStatus(http.StatusCreated)

	bOrder := psOrderID(t, srv, "B-1", "tenant-b", asB)
	bPayment := srv.MustID(srv.POST("/ps_payments", map[string]any{
		"ps_order_id": bOrder, "amount": 7, "org_id": "tenant-b"}, asB))

	srv.PATCH("/ps_payments/"+bPayment, map[string]any{"ps_order_id": aOrder}, asB).
		AssertStatus(http.StatusNotFound)

	if got := psPaid(t, srv, aOrder, asA); got != 100 {
		t.Errorf("A's rollup moved to %v on B's re-parenting PATCH", got)
	}
	if got := psPaid(t, srv, bOrder, asB); got != 7 {
		t.Errorf("B's own rollup = %v, want 7 — the refused PATCH must change nothing", got)
	}
}

// The junction write side, which is the same key check reaching QRY-12's other
// half: a link row is a model with two BelongsTo relations, so a link between
// two of A's records is now refused rather than merely hidden on read.
func TestParentScope_ForeignJunctionLinkIsRefused(t *testing.T) {
	t.Parallel()
	srv := scopedJunctionSrv(t)

	sid := srv.MustID(srv.POST("/sc_students", map[string]any{"name": "Ada", "org_id": "tenant-a"}, asA))
	cid := srv.MustID(srv.POST("/sc_courses", map[string]any{"title": "Logic", "org_id": "tenant-a"}, asA))

	srv.POST("/sc_enrols", map[string]any{
		"sc_student_id": sid, "sc_course_id": cid, "grade": foreignGrade,
	}, asB).AssertStatus(http.StatusNotFound)
}

// A scope built in Go naturally reaches for the name the model publishes, which
// is the json one — db.Tenancy hands its field to ctx.SetField, whose parameter
// is the json name. Gating the check on the DB spelling alone would silently
// skip it for that shape, which is how MS-9 stayed open on the read side for a
// release (audit O2).
type psJSpaceOrder struct {
	maniflex.BaseModel
	Reference string `json:"reference"`
	OrgID     string `json:"orgId" db:"org_id"`
}

type psJSpaceNote struct {
	maniflex.BaseModel
	Body            string         `json:"body"`
	PsJSpaceOrderID string         `json:"psJSpaceOrderId" db:"ps_j_space_order_id" mfx:"relation"`
	PsJSpaceOrder   *psJSpaceOrder `json:"psJSpaceOrder,omitempty"`
	OrgID           string         `json:"orgId" db:"org_id"`
}

func TestParentScope_ScopeDeclaredByJSONNameIsStillEnforced(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{psJSpaceOrder{}, psJSpaceNote{}},
		Middleware: func(s *maniflex.Server) {
			// "orgId", the json spelling — the column is org_id.
			jsonScope := dbmw.ForceFilter("orgId", func(ctx *maniflex.ServerContext) any {
				if org := ctx.Request.Header.Get("X-Org"); org != "" {
					return org
				}
				return nil
			})
			for _, m := range []string{"psJSpaceOrder", "psJSpaceNote"} {
				s.Pipeline.DB.Register(jsonScope, maniflex.ForModel(m))
			}
		},
	})

	oid := srv.MustID(srv.POST("/ps_j_space_orders",
		map[string]any{"reference": "A-1", "orgId": "tenant-a"}, asA))

	srv.POST("/ps_j_space_notes", map[string]any{
		"body": "planted", "psJSpaceOrderId": oid, "orgId": "tenant-b"}, asB).
		AssertStatus(http.StatusNotFound)

	// A's own write through the same scope still lands.
	srv.POST("/ps_j_space_notes", map[string]any{
		"body": "mine", "psJSpaceOrderId": oid, "orgId": "tenant-a"}, asA).
		AssertStatus(http.StatusCreated)
}

// ── what must still work ─────────────────────────────────────────────────────

func TestParentScope_LegitimateWritesAreUntouched(t *testing.T) {
	t.Parallel()
	srv := psSrv(t)

	oid := psOrderID(t, srv, "A-1", "tenant-a", asA)
	cur := srv.MustID(srv.POST("/ps_currencies", map[string]any{"code": "EUR"}))

	t.Run("own_parent", func(t *testing.T) {
		srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "amount": 40, "org_id": "tenant-a"}, asA).
			AssertStatus(http.StatusCreated)
	})

	t.Run("shared_parent_carrying_no_scope_column", func(t *testing.T) {
		// psCurrency has no org_id, so the scope says nothing about it and a
		// payment in any tenant may name it. Refusing this would 404 every write
		// that references a lookup table.
		srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "ps_currency_id": cur,
			"amount": 5, "org_id": "tenant-a"}, asA).
			AssertStatus(http.StatusCreated)
	})

	t.Run("convention_fk_to_an_unregistered_model", func(t *testing.T) {
		srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "ps_ledger_id": "ledger-from-another-service",
			"amount": 5, "org_id": "tenant-a"}, asA).
			AssertStatus(http.StatusCreated)
	})

	t.Run("empty_key_is_legal_under_a_flat_scope", func(t *testing.T) {
		// Unlike a scope that runs through the parent, where a null key is a 422
		// because the row would be invisible to its own author: here the child
		// carries org_id itself, so a payment with no currency is perfectly
		// describable.
		srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "ps_currency_id": "",
			"amount": 5, "org_id": "tenant-a"}, asA).
			AssertStatus(http.StatusCreated)
	})

	t.Run("patch_that_leaves_the_key_alone", func(t *testing.T) {
		id := srv.MustID(srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "amount": 11, "org_id": "tenant-a"}, asA))
		srv.PATCH("/ps_payments/"+id, map[string]any{"amount": 12}, asA).
			AssertStatus(http.StatusOK)
	})

	t.Run("unscoped_request_checks_nothing", func(t *testing.T) {
		// orgScope() imposes no filter without the header, so there is no scope
		// to measure a parent against — and no read is issued for one.
		srv.POST("/ps_payments", map[string]any{
			"ps_order_id": oid, "amount": 3, "org_id": "tenant-b"}).
			AssertStatus(http.StatusCreated)
	})
}
