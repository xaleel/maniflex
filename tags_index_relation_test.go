package maniflex_test

// Coverage for two registration-time tag directives:
//   - mfx:"index"    → an IndexSpec is appended so AutoMigrate creates the index (§10.4)
//   - mfx:"relation" → an FK field opts IN to a BelongsTo relation; untagged
//     "<Name>ID" fields are plain scalar columns (relations are no longer inferred
//     from the "ID" suffix alone).
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
	OwnerID string `json:"owner_id" mfx:"index"` // plain scalar FK column + index
}

func TestIndexTag_OnFKColumn(t *testing.T) {
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

// ── mfx:"relation" opt-in (and untagged *ID is NOT a relation) ────────────────

type relInferModel struct {
	maniflex.BaseModel
	UserID   string `json:"user_id"`                    // untagged → plain scalar, NO relation
	AuthorID string `json:"author_id" mfx:"relation"`   // opt IN → BelongsTo "author"
	TeamID   string `json:"team_id"   mfx:"norelation"` // deprecated no-op → scalar
}

func TestRelation_OptInOnly(t *testing.T) {
	meta := scan(t, relInferModel{})

	// Untagged UserID no longer auto-relates — it's just a column.
	if rel := meta.RelationByKey("user"); rel != nil {
		t.Errorf("untagged UserID must NOT produce a relation, got %+v", rel)
	}
	if f := meta.FieldByDBName("user_id"); f == nil {
		t.Error("user_id should still be a scalar column")
	}

	// mfx:"relation" opts in: AuthorID → BelongsTo Author (target inferred by
	// stripping the ID suffix).
	rel := meta.RelationByKey("author")
	if rel == nil {
		t.Fatal(`AuthorID mfx:"relation" should produce a BelongsTo relation "author"`)
	}
	if rel.Kind != maniflex.BelongsTo || rel.RelatedModel != "Author" {
		t.Errorf("author relation wrong: kind=%v target=%q", rel.Kind, rel.RelatedModel)
	}
	if f := meta.FieldByDBName("author_id"); f == nil {
		t.Error("author_id FK column should exist")
	}

	// norelation is now a harmless no-op: still a scalar, no relation, no error.
	if rel := meta.RelationByKey("team"); rel != nil {
		t.Errorf("norelation TeamID must not relate, got %+v", rel)
	}
	if f := meta.FieldByDBName("team_id"); f == nil {
		t.Error("team_id should remain a scalar column")
	}
}
