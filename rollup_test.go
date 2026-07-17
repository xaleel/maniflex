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
