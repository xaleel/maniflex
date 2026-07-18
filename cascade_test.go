package maniflex

import (
	"reflect"
	"strings"
	"testing"
)

// belongsTo builds a BelongsTo relation carrying an onDelete action, as the
// tag parser would produce it.
func belongsTo(related, fkCol, key string, action OnDeleteAction) RelationMeta {
	return RelationMeta{
		Kind:         BelongsTo,
		RelatedModel: related,
		FKColumn:     fkCol,
		RelationKey:  key,
		OnDelete:     action,
	}
}

func TestChildCascadeEdges(t *testing.T) {
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{Name: "Author"})
	mustAdd(t, reg, &ModelMeta{Name: "Post", Relations: []RelationMeta{
		belongsTo("Author", "author_id", "author", OnDeleteCascade),
	}})
	mustAdd(t, reg, &ModelMeta{Name: "Comment", Relations: []RelationMeta{
		belongsTo("Post", "post_id", "post", OnDeleteSetNull),
	}})
	// A relation with no action must not be an edge, and neither must a plain field.
	mustAdd(t, reg, &ModelMeta{Name: "Tag", Relations: []RelationMeta{
		belongsTo("Author", "author_id", "author", OnDeleteNoAction),
	}})

	authorEdges := childCascadeEdges(reg, "Author")
	if len(authorEdges) != 1 || authorEdges[0].child.Name != "Post" {
		t.Fatalf("childCascadeEdges(Author) = %v, want one edge from Post", edgeNames(authorEdges))
	}
	if got := childCascadeEdges(reg, "Post"); len(got) != 1 || got[0].child.Name != "Comment" {
		t.Fatalf("childCascadeEdges(Post) = %v, want one edge from Comment", edgeNames(got))
	}
	if got := childCascadeEdges(reg, "Nobody"); len(got) != 0 {
		t.Fatalf("childCascadeEdges(Nobody) = %v, want none", edgeNames(got))
	}
}

func TestDbEnforcedDelete(t *testing.T) {
	hard := &ModelMeta{Name: "Hard"}
	soft := &ModelMeta{Name: "Soft", SoftDelete: SoftDeleteConfig{Enabled: true, Field: "deleted_at"}}

	if !dbEnforcedDelete(hard, hard) {
		t.Error("hard→hard should be DB-enforced")
	}
	if dbEnforcedDelete(soft, hard) {
		t.Error("soft parent must fall to app-layer (ON DELETE never fires on a soft delete)")
	}
	if dbEnforcedDelete(hard, soft) {
		t.Error("soft child must fall to app-layer (a DB cascade can only hard-delete)")
	}
	if dbEnforcedDelete(soft, soft) {
		t.Error("soft→soft must fall to app-layer")
	}
}

func TestValidateOnDeleteActions_SetNullRequiresNullableFK(t *testing.T) {
	// A non-pointer FK is NOT NULL, so setNull cannot write NULL into it.
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{Name: "Author"})
	mustAdd(t, reg, &ModelMeta{
		Name:      "Post",
		Fields:    []FieldMeta{{Name: "AuthorID", Type: reflect.TypeOf(""), Tags: FieldTags{DBName: "author_id"}}},
		Relations: []RelationMeta{belongsTo("Author", "author_id", "author", OnDeleteSetNull)},
	})
	err := validateOnDeleteActions(reg)
	if err == nil {
		t.Fatal("validateOnDeleteActions accepted setNull on a NOT NULL FK")
	}
	if !strings.Contains(err.Error(), "setNull") || !strings.Contains(err.Error(), "author_id") {
		t.Errorf("error should name the action and FK column: %v", err)
	}

	// A pointer FK is nullable, so setNull is valid.
	reg2 := NewRegistry()
	mustAdd(t, reg2, &ModelMeta{Name: "Author"})
	mustAdd(t, reg2, &ModelMeta{
		Name:      "Post",
		Fields:    []FieldMeta{{Name: "AuthorID", Type: reflect.TypeOf((*string)(nil)), Tags: FieldTags{DBName: "author_id"}}},
		Relations: []RelationMeta{belongsTo("Author", "author_id", "author", OnDeleteSetNull)},
	})
	if err := validateOnDeleteActions(reg2); err != nil {
		t.Errorf("validateOnDeleteActions rejected setNull on a nullable (*string) FK: %v", err)
	}
}

func TestValidateOnDeleteActions_UnregisteredTarget(t *testing.T) {
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{
		Name:      "Post",
		Fields:    []FieldMeta{{Name: "AuthorID", Type: reflect.TypeOf(""), Tags: FieldTags{DBName: "author_id"}}},
		Relations: []RelationMeta{belongsTo("Author", "author_id", "author", OnDeleteCascade)},
	})
	err := validateOnDeleteActions(reg)
	if err == nil {
		t.Fatal("validateOnDeleteActions accepted onDelete against an unregistered target")
	}
	if !strings.Contains(err.Error(), "Author") || !strings.Contains(err.Error(), "not") {
		t.Errorf("error should name the missing target: %v", err)
	}
}

func TestValidateOnDeleteActions_ValidPasses(t *testing.T) {
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{Name: "Author"})
	mustAdd(t, reg, &ModelMeta{Name: "Post", Relations: []RelationMeta{
		belongsTo("Author", "author_id", "author", OnDeleteCascade),
	}})
	mustAdd(t, reg, &ModelMeta{
		Name:      "Comment",
		Fields:    []FieldMeta{{Name: "PostID", Type: reflect.TypeOf((*string)(nil)), Tags: FieldTags{DBName: "post_id"}}},
		Relations: []RelationMeta{belongsTo("Post", "post_id", "post", OnDeleteSetNull)},
	})
	mustAdd(t, reg, &ModelMeta{Name: "Audit", Relations: []RelationMeta{
		belongsTo("Author", "author_id", "author", OnDeleteRestrict),
	}})
	if err := validateOnDeleteActions(reg); err != nil {
		t.Errorf("validateOnDeleteActions rejected a valid set of actions: %v", err)
	}
}

func TestForeignKeysFor(t *testing.T) {
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{Name: "Author", TableName: "authors"})
	mustAdd(t, reg, &ModelMeta{Name: "SoftTeam", TableName: "soft_teams",
		SoftDelete: SoftDeleteConfig{Enabled: true, Field: "deleted_at"}})
	// hard child of hard parent → the database can enforce it.
	mustAdd(t, reg, &ModelMeta{Name: "Post", TableName: "posts", Relations: []RelationMeta{
		belongsTo("Author", "author_id", "author", OnDeleteCascade),
	}})
	// hard child of a soft parent → app-enforced, no FK.
	mustAdd(t, reg, &ModelMeta{Name: "Note", TableName: "notes", Relations: []RelationMeta{
		belongsTo("SoftTeam", "team_id", "team", OnDeleteCascade),
	}})
	// soft child → app-enforced, no FK.
	mustAdd(t, reg, &ModelMeta{Name: "SoftDoc", TableName: "soft_docs",
		SoftDelete: SoftDeleteConfig{Enabled: true, Field: "deleted_at"},
		Relations:  []RelationMeta{belongsTo("Author", "author_id", "author", OnDeleteSetNull)}})

	post, _ := reg.Get("Post")
	fks := ForeignKeysFor(reg, post)
	if len(fks) != 1 {
		t.Fatalf("Post FKs = %d, want 1 (hard→hard)", len(fks))
	}
	if fk := fks[0]; fk.Column != "author_id" || fk.RefTable != "authors" ||
		fk.RefColumn != "id" || fk.OnDelete != OnDeleteCascade {
		t.Errorf("unexpected FK spec: %+v", fk)
	}

	note, _ := reg.Get("Note")
	if got := ForeignKeysFor(reg, note); len(got) != 0 {
		t.Errorf("Note (soft parent) FKs = %d, want 0 — app-enforced", len(got))
	}
	softDoc, _ := reg.Get("SoftDoc")
	if got := ForeignKeysFor(reg, softDoc); len(got) != 0 {
		t.Errorf("SoftDoc (soft child) FKs = %d, want 0 — app-enforced", len(got))
	}
}

func mustAdd(t *testing.T, reg *Registry, m *ModelMeta) {
	t.Helper()
	if err := reg.AddForTest(m); err != nil {
		t.Fatalf("AddForTest(%s): %v", m.Name, err)
	}
}

func edgeNames(edges []cascadeEdge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.child.Name
	}
	return out
}
