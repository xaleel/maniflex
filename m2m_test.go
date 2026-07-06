package maniflex_test

import (
	"testing"

	"github.com/xaleel/maniflex"
)

// ── Scanner detection tests ───────────────────────────────────────────────────

// Minimal models that form a clean two-FK junction.
type m2mProduct struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
	Tags []m2mTag `json:"tags,omitempty" mfx:"through:M2MProductTag"`
}
type m2mTag struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
	// No explicit Products field — auto-detection should wire this up as M2M.
}
type M2MProductTag struct {
	maniflex.BaseModel
	ProductID  string `json:"product_id"  db:"product_id"  mfx:"required,relation"`
	TagID      string `json:"tag_id"      db:"tag_id"      mfx:"required,relation"`
	AssignedBy string `json:"assigned_by" db:"assigned_by"`
	Product    m2mProduct
	Tag        m2mTag
}

func registerM2MModels(t *testing.T) *maniflex.Registry {
	t.Helper()
	reg := maniflex.NewRegistry()
	for _, v := range []any{m2mProduct{}, m2mTag{}, M2MProductTag{}} {
		meta, err := maniflex.ScanModel(v, maniflex.ModelConfig{})
		if err != nil {
			t.Fatalf("ScanModel: %v", err)
		}
		if err := reg.AddForTest(meta); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := maniflex.ResolveManyToManyForTest(reg); err != nil {
		t.Fatalf("resolveManyToMany: %v", err)
	}
	return reg
}

func TestM2M_ExplicitThroughTag_Resolved(t *testing.T) {
	reg := registerM2MModels(t)
	meta, _ := reg.Get("m2mProduct")

	rel := meta.RelationByKey("tags")
	if rel == nil {
		t.Fatal("expected 'tags' relation on m2mProduct")
	}
	if rel.Kind != maniflex.ManyToMany {
		t.Fatalf("expected ManyToMany, got %v", rel.Kind)
	}
	if rel.ThroughTable == "" {
		t.Fatal("ThroughTable not set")
	}
	if rel.ThroughLocalFK == "" || rel.ThroughRemoteFK == "" {
		t.Fatalf("through FKs not resolved: local=%q remote=%q", rel.ThroughLocalFK, rel.ThroughRemoteFK)
	}
}

func TestM2M_AutoDetect_BidirectionalOnTag(t *testing.T) {
	reg := registerM2MModels(t)
	tagMeta, _ := reg.Get("m2mTag")

	// m2mTag has no explicit through: tag, but auto-detection should wire up
	// an m2mProducts relation from the junction
	rel := tagMeta.RelationByModel("m2mProduct")
	if rel == nil {
		t.Fatal("expected auto-detected M2M relation on m2mTag → m2mProduct")
	}
	if rel.Kind != maniflex.ManyToMany {
		t.Fatalf("expected ManyToMany, got %v", rel.Kind)
	}
}

func TestM2M_ThreeFKJunctionNotAutoDetected(t *testing.T) {
	// A junction with 3 BelongsTo relations should NOT be auto-detected as M2M.
	type A struct{ maniflex.BaseModel }
	type B struct{ maniflex.BaseModel }
	type C struct{ maniflex.BaseModel }
	type ABC struct {
		maniflex.BaseModel
		AID string `db:"a_id"`
		BID string `db:"b_id"`
		CID string `db:"c_id"`
		Av  A
		Bv  B
		Cv  C
	}

	reg := maniflex.NewRegistry()
	for _, v := range []any{A{}, B{}, C{}, ABC{}} {
		meta, err := maniflex.ScanModel(v, maniflex.ModelConfig{})
		if err != nil {
			t.Fatalf("ScanModel: %v", err)
		}
		_ = reg.AddForTest(meta)
	}
	if err := maniflex.ResolveManyToManyForTest(reg); err != nil {
		t.Fatal(err)
	}

	// Neither A nor B should have a ManyToMany relation via ABC
	aMeta, _ := reg.Get("A")
	for _, r := range aMeta.Relations {
		if r.Kind == maniflex.ManyToMany {
			t.Errorf("unexpected ManyToMany on A: %+v", r)
		}
	}
}

func TestM2M_ExplicitThroughUnregisteredModel_Error(t *testing.T) {
	type Orphan struct {
		maniflex.BaseModel
		Name  string  `json:"name" db:"name"`
		Items []m2mTag `json:"items,omitempty" mfx:"through:DoesNotExist"`
	}

	reg := maniflex.NewRegistry()
	for _, v := range []any{Orphan{}, m2mTag{}} {
		meta, err := maniflex.ScanModel(v, maniflex.ModelConfig{})
		if err != nil {
			t.Fatalf("ScanModel: %v", err)
		}
		_ = reg.AddForTest(meta)
	}
	if err := maniflex.ResolveManyToManyForTest(reg); err == nil {
		t.Fatal("expected error for unregistered junction model")
	}
}
