package maniflex

// R12 — Rollup registration validation. The whole argument for the typed config
// over a struct-tag mini-language is that a mistake is a startup error naming the
// field, not a silently drifted total. These tests hold that line.

import (
	"strings"
	"testing"
)

type RollupParentT struct {
	BaseModel
	Total int `json:"total" db:"total" mfx:"default:0"`
}

type RollupChildT struct {
	BaseModel
	ParentID string `json:"parent_id" db:"parent_id" mfx:"required,filterable"`
	Amount   int    `json:"amount"    db:"amount"    mfx:"required"`
	Status   string `json:"status"    db:"status"    mfx:"filterable"`
	Cap      int    `json:"cap"       db:"cap"`
}

func rollupTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(Config{})
	if err := s.Register(RollupParentT{}, RollupChildT{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}

func validRollup() Rollup {
	return Rollup{
		Parent: "RollupParentT", ParentField: "total", Op: AggSum,
		Child: "RollupChildT", ChildField: "amount", On: "parent_id",
	}
}

func TestRollup_ValidRegisters(t *testing.T) {
	s := rollupTestServer(t)
	if err := s.RegisterRollup(validRollup()); err != nil {
		t.Fatalf("a valid rollup must register: %v", err)
	}
}

func TestRollup_CountMayOmitChildField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Op = AggCount
	r.ChildField = ""
	if err := s.RegisterRollup(r); err != nil {
		t.Errorf("AggCount without ChildField must register: %v", err)
	}
}

func TestRollup_RejectsUnknownParentModel(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Parent = "Nope"
	assertRollupErr(t, s.RegisterRollup(r), "Nope")
}

func TestRollup_RejectsUnknownChildModel(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Child = "Nope"
	assertRollupErr(t, s.RegisterRollup(r), "Nope")
}

// The core promise: a typo in a field name is a startup error, not a runtime
// no-op. This is the whole case for the typed config over a tag mini-language.
func TestRollup_RejectsUnknownParentField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.ParentField = "totl" // typo
	assertRollupErr(t, s.RegisterRollup(r), "totl")
}

func TestRollup_RejectsUnknownChildField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.ChildField = "amont" // typo
	assertRollupErr(t, s.RegisterRollup(r), "amont")
}

func TestRollup_RejectsUnknownOnField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.On = "parnt_id" // typo
	assertRollupErr(t, s.RegisterRollup(r), "parnt_id")
}

func TestRollup_RejectsMissingChildFieldForSum(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.ChildField = ""
	assertRollupErr(t, s.RegisterRollup(r), "ChildField")
}

func TestRollup_RejectsUnsupportedOp(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Op = AggCountDistinct
	assertRollupErr(t, s.RegisterRollup(r), "aggregate")
}

func TestRollup_RejectsEmptyRequiredFields(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.On = ""
	assertRollupErr(t, s.RegisterRollup(r), "On")
}

func TestRollup_MustRegisterPanicsOnBadConfig(t *testing.T) {
	s := rollupTestServer(t)
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegisterRollup must panic on an invalid config")
		}
	}()
	r := validRollup()
	r.ParentField = "totl"
	s.MustRegisterRollup(r)
}

// ── Where: the child filter ─────────────────────────────────────────────────

func TestRollup_WhereRegisters(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{
		{Field: "status", Operator: OpEq, Value: "captured"}, // json name
		{Field: "amount", Operator: OpGt, Value: 0},          // db name
		{Field: "amount", Operator: OpLteField, ValueField: "cap"},
	}
	if err := s.RegisterRollup(r); err != nil {
		t.Fatalf("a valid Where must register: %v", err)
	}
}

// The same promise the field names make: a typo is a startup error naming it,
// not a filter that fails closed and holds the column at 0 forever.
func TestRollup_WhereRejectsUnknownField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{{Field: "staus", Operator: OpEq, Value: "captured"}}
	assertRollupErr(t, s.RegisterRollup(r), "staus")
}

func TestRollup_WhereRejectsUnknownValueField(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{{Field: "amount", Operator: OpLteField, ValueField: "capp"}}
	assertRollupErr(t, s.RegisterRollup(r), "capp")
}

func TestRollup_WhereRejectsUnknownOperator(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{{Field: "status", Operator: "equals", Value: "captured"}}
	assertRollupErr(t, s.RegisterRollup(r), "equals")
}

// Nested and locale filters resolve against a joined table or a JSON key; the
// recompute aggregates the child table alone, so both are refused up front
// rather than surfacing as a 500 on whichever child write runs first.
func TestRollup_WhereRejectsNestedFilter(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{{
		Field: "owner.status", Operator: OpEq, Value: "active",
		IsNested: true, RelationKey: "owner", NestedField: "status",
	}}
	assertRollupErr(t, s.RegisterRollup(r), "nested-relation")
}

func TestRollup_WhereRejectsLocaleFilter(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{{
		Field: "status.ar", Operator: OpEq, Value: "مسدد",
		IsLocale: true, LocaleKey: "ar",
	}}
	assertRollupErr(t, s.RegisterRollup(r), "locale")
}

func TestRollup_WhereRejectsNilFilter(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	r.Where = []*FilterExpr{nil}
	assertRollupErr(t, s.RegisterRollup(r), "nil filter")
}

// The compiled rollup holds its own array. A caller reusing their slice — the
// same one filled in again for a second rollup — must not rewrite what an
// already-registered one aggregates.
func TestRollup_WhereIsCopiedAtRegistration(t *testing.T) {
	s := rollupTestServer(t)
	r := validRollup()
	filters := []*FilterExpr{{Field: "status", Operator: OpEq, Value: "captured"}}
	r.Where = filters
	if err := s.RegisterRollup(r); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Reusing the slot: without the copy this is the compiled filter too.
	filters[0] = &FilterExpr{Field: "amount", Operator: OpGt, Value: 1000}

	if got := s.rollups[0].where[0].Field; got != "status" {
		t.Errorf("compiled Where field: got %q, want %q (the caller's slice was reused after "+
			"registration and must not reach the compiled rollup)", got, "status")
	}
}

func assertRollupErr(t *testing.T, err error, mustName string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a registration error naming %q, got nil", mustName)
	}
	if !strings.Contains(err.Error(), mustName) {
		t.Errorf("error must name %q; got: %v", mustName, err)
	}
}

// softDeleteFilter must produce the right predicate per style.
func TestRollup_SoftDeleteFilterShape(t *testing.T) {
	ts := softDeleteFilter(SoftDeleteConfig{Enabled: true, Field: "deleted_at", FieldType: SoftDeleteTimestamp})
	if ts.Operator != OpIsNull || ts.Field != "deleted_at" {
		t.Errorf("timestamp soft-delete filter: got %+v, want deleted_at IS NULL", ts)
	}
	b := softDeleteFilter(SoftDeleteConfig{Enabled: true, Field: "is_deleted", FieldType: SoftDeleteBool})
	if b.Operator != OpEq || b.Value != false {
		t.Errorf("bool soft-delete filter: got %+v, want is_deleted = false", b)
	}
}
