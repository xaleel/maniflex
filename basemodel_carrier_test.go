package maniflex

// Phase 1 / T1.1 PoD: the framework-internal BaseModel carriers and the
// recordMeta contract are additive — they must not change registration output
// (Fields/Indices) and must be reachable through a *T type assertion.

import (
	"reflect"
	"testing"
)

type carrierModel struct {
	BaseModel
	Name   string `json:"name"`
	Age    int    `json:"age"`
	UserID string `json:"user_id"` // convention FK → relation, also a real column
}

// TestBaseModel_SatisfiesRecordMeta proves a *T (embedding BaseModel by value)
// is reachable as recordMeta and the methods round-trip through that assertion.
func TestBaseModel_SatisfiesRecordMeta(t *testing.T) {
	rm, ok := any(&carrierModel{}).(recordMeta)
	if !ok {
		t.Fatal("*carrierModel does not satisfy recordMeta")
	}

	if rm.mfxPresent() != nil {
		t.Error("fresh record should have nil present set")
	}
	keys := map[string]struct{}{"name": {}, "age": {}}
	rm.mfxSetPresent(keys)
	if got := rm.mfxPresent(); len(got) != 2 {
		t.Errorf("present = %v, want 2 keys", got)
	}

	// mfxExtra allocates lazily and returns the same map on subsequent calls.
	e := rm.mfxExtra()
	if e == nil {
		t.Fatal("mfxExtra returned nil")
	}
	e["computed"] = 42
	if rm.mfxExtra()["computed"] != 42 {
		t.Error("mfxExtra did not return a stable, writable map")
	}

	if rm.mfxSelect() != nil {
		t.Error("fresh record should have nil select set")
	}
	rm.mfxSetSelect(map[string]struct{}{"name": {}})
	if _, ok := rm.mfxSelect()["name"]; !ok {
		t.Error("mfxSetSelect/mfxSelect did not round-trip")
	}
}

// TestScanModel_CarriersAreNotColumns proves the unexported carriers never
// surface as DB-column fields and the registration output is unchanged.
func TestScanModel_CarriersAreNotColumns(t *testing.T) {
	meta, err := ScanModel(carrierModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}

	for _, f := range meta.Fields {
		switch f.Name {
		case "present", "extra", "selectFn":
			t.Errorf("carrier %q leaked into ModelMeta.Fields", f.Name)
		}
	}

	// Golden: the exact column set is id, created_at, updated_at, name, age,
	// user_id — the carriers add none.
	want := map[string]bool{
		"id": true, "created_at": true, "updated_at": true,
		"name": true, "age": true, "user_id": true,
	}
	got := map[string]bool{}
	for _, f := range meta.Fields {
		got[f.Tags.DBName] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}

	// Golden: index paths of the embedded BaseModel columns are unchanged —
	// adding the carriers *after* the exported fields must not shift [0..2].
	for db, wantIdx := range map[string][]int{
		"id": {0, 0}, "created_at": {0, 1}, "updated_at": {0, 2},
	} {
		f := meta.FieldByDBName(db)
		if f == nil {
			t.Fatalf("missing column %q", db)
		}
		if !reflect.DeepEqual(f.Index, wantIdx) {
			t.Errorf("%s Index = %v, want %v", db, f.Index, wantIdx)
		}
	}
}
