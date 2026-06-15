package maniflex_test

// Coverage for two registration-time tag directives:
//   - mfx:"index"      → an IndexSpec is appended so AutoMigrate creates the index (§10.4)
//   - mfx:"norelation" → a convention-FK field stays a plain scalar column (§10.2)
//
// Both are resolved entirely by ScanModel, so these are fast meta-level tests.
// hasIndexOnColumn / scan / captureWarnings are shared with scheduled_test.go.

import (
	"testing"

	"github.com/xaleel/maniflex"
)

// ── 10.4: mfx:"index" ──────────────────────────────────────────────────────────

type indexedModel struct {
	maniflex.BaseModel
	Slug  string `json:"slug"  mfx:"index"`
	Email string `json:"email" mfx:"unique,index"` // unique already indexes implicitly
	Name  string `json:"name"`                     // no index
}

func TestIndexTag_AppendsIndexSpec(t *testing.T) {
	meta := scan(t, indexedModel{})

	if n := hasIndexOnColumn(meta, "slug"); n != 1 {
		t.Errorf("expected exactly one index on 'slug', got %d", n)
	}
	// A unique column is already indexed by the UNIQUE constraint, so mfx:"index"
	// must not add a redundant second index.
	if n := hasIndexOnColumn(meta, "email"); n != 0 {
		t.Errorf("expected no extra index on unique 'email', got %d", n)
	}
	if n := hasIndexOnColumn(meta, "name"); n != 0 {
		t.Errorf("expected no index on untagged 'name', got %d", n)
	}

	// The Index tag is also surfaced on the field's parsed tags.
	if f := meta.FieldByDBName("slug"); f == nil || !f.Tags.Index {
		t.Errorf("slug field should carry Tags.Index = true")
	}
}

type indexedFKModel struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id" mfx:"index"` // convention FK + index on the FK column
}

func TestIndexTag_OnConventionFKColumn(t *testing.T) {
	meta := scan(t, indexedFKModel{})
	// The FK column is a real DB column, so indexing it is valid and useful.
	if n := hasIndexOnColumn(meta, "owner_id"); n != 1 {
		t.Errorf("expected one index on FK column 'owner_id', got %d", n)
	}
}

func TestIndexTag_NotDuplicatedWhenUserDeclared(t *testing.T) {
	meta, err := maniflex.ScanModel(indexedModel{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{{Name: "my_slug_idx", Columns: []string{"slug"}}},
	})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	if n := hasIndexOnColumn(meta, "slug"); n != 1 {
		t.Errorf("expected the single user-declared index on 'slug', got %d", n)
	}
}

// ── 10.2: mfx:"norelation" ──────────────────────────────────────────────────────

type relOptOutModel struct {
	maniflex.BaseModel
	UserID string `json:"user_id"`                  // convention FK → relation "user"
	TeamID string `json:"team_id" mfx:"norelation"` // opted out → scalar only
}

func TestNoRelation_OptsOutConventionFK(t *testing.T) {
	meta := scan(t, relOptOutModel{})

	// UserID still produces the convention relation.
	if rel := meta.RelationByKey("user"); rel == nil {
		t.Error("UserID should still produce a convention BelongsTo relation 'user'")
	}

	// TeamID does NOT produce a relation...
	if rel := meta.RelationByKey("team"); rel != nil {
		t.Errorf("TeamID mfx:\"norelation\" must not produce a relation, got %+v", rel)
	}
	if rel := meta.RelationByModel("Team"); rel != nil {
		t.Errorf("TeamID mfx:\"norelation\" must not reference a Team model, got %+v", rel)
	}

	// ...but it IS still a plain scalar DB column.
	if f := meta.FieldByDBName("team_id"); f == nil {
		t.Error("team_id should remain a scalar column even when opted out of the relation")
	} else if !f.Tags.NoRelation {
		t.Error("team_id field should carry Tags.NoRelation = true")
	}

	// The FK column for the kept relation is still present too.
	if f := meta.FieldByDBName("user_id"); f == nil {
		t.Error("user_id scalar column missing")
	}
}

// norelation on a non-convention field (one that never implied a relation) is a
// harmless no-op: the field stays a scalar, no relation appears or disappears.
type relOptOutNoopModel struct {
	maniflex.BaseModel
	Title string `json:"title" mfx:"norelation"`
}

func TestNoRelation_NoopOnPlainField(t *testing.T) {
	meta := scan(t, relOptOutNoopModel{})
	if len(meta.Relations) != 0 {
		t.Errorf("expected no relations, got %d", len(meta.Relations))
	}
	if f := meta.FieldByDBName("title"); f == nil {
		t.Error("title should remain a scalar column")
	}
}
