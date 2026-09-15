package maniflex

// Audit PIPE-4 — dbEnforcedDelete decides whether an onDelete edge is left to a
// database FK constraint or walked by maniflex, and it is the one place that
// decision is made: ForeignKeysFor emits a constraint for exactly the edges
// cascadeChildren skips.
//
// It used to turn on soft-delete alone. A database ON DELETE clause removes the
// rows and tells nobody, so a versioned child lost its delete history and a
// rollup child left its parent's total counting rows that were gone. Bookkeeping
// now keeps an edge in the framework too.
//
// These are here rather than end-to-end because the DDL half is only observable
// here: the test harness migrates before rollups are registered, so a constraint
// it emits says nothing about what a real Start() would emit.

import "testing"

func cascadeParent() *ModelMeta { return &ModelMeta{Name: "Parent"} }

func TestDBEnforcedDelete_PlainChildIsLeftToTheDatabase(t *testing.T) {
	// The case that must not change: nothing to do beyond deleting the row, so
	// one ON DELETE clause is cheaper than a paged walk.
	if !dbEnforcedDelete(cascadeParent(), &ModelMeta{Name: "Plain"}) {
		t.Error("a child with no soft-delete, no versioning and no rollup must stay a database edge")
	}
}

func TestDBEnforcedDelete_BookkeepingKeepsTheEdgeInTheFramework(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child *ModelMeta
	}{
		{"versioned child", &ModelMeta{Name: "Versioned", Config: ModelConfig{Versioned: true}}},
		{"rollup child", &ModelMeta{Name: "Rolled", rollupChild: true}},
		{"soft-delete child", &ModelMeta{Name: "Soft", SoftDelete: SoftDeleteConfig{Enabled: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if dbEnforcedDelete(cascadeParent(), tc.child) {
				t.Error("the database would delete these rows and tell nobody; " +
					"the edge must be walked by maniflex so the child's bookkeeping runs")
			}
		})
	}
}

func TestDBEnforcedDelete_SoftDeleteParentStillWins(t *testing.T) {
	soft := &ModelMeta{Name: "Parent", SoftDelete: SoftDeleteConfig{Enabled: true}}
	if dbEnforcedDelete(soft, &ModelMeta{Name: "Plain"}) {
		t.Error("a soft delete is an UPDATE, so no ON DELETE clause ever fires for it")
	}
}

// The invariant the file's own comment states: the constraints an adapter emits
// and the edges the walk skips are the same set, drawn by the same line. A
// versioned child must therefore get no ON DELETE clause, or the database would
// cascade behind the framework's back.
func TestForeignKeysFor_NoConstraintForABookkeepingChild(t *testing.T) {
	reg := NewRegistry()
	mustAdd(t, reg, &ModelMeta{Name: "Parent", TableName: "parents"})

	plain := &ModelMeta{Name: "Plain", TableName: "plains", Relations: []RelationMeta{
		belongsTo("Parent", "parent_id", "parent", OnDeleteCascade),
	}}
	mustAdd(t, reg, plain)
	if got := ForeignKeysFor(reg, plain); len(got) != 1 || got[0].OnDelete != OnDeleteCascade {
		t.Fatalf("plain child got %d constraint(s), want one ON DELETE CASCADE", len(got))
	}

	versioned := &ModelMeta{
		Name: "Versioned", TableName: "versioneds",
		Config:    ModelConfig{Versioned: true},
		Relations: []RelationMeta{belongsTo("Parent", "parent_id", "parent", OnDeleteCascade)},
	}
	mustAdd(t, reg, versioned)
	if got := ForeignKeysFor(reg, versioned); len(got) != 0 {
		t.Errorf("versioned child got %d constraint(s), want none — the database would "+
			"cascade behind the framework and the history would lose its delete rows", len(got))
	}
}

// foreignKeyID exists because a nullable FK — the shape onDelete:setNull
// requires — reaches the rollup bookkeeping as a *string, and fmt.Sprint on that
// yields a pointer address, which names no row.
func TestForeignKeyID(t *testing.T) {
	id := "abc"
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"string", "abc", "abc"},
		{"pointer to string", &id, "abc"},
		{"nil pointer", (*string)(nil), ""},
		{"nil", nil, ""},
		{"int", 7, "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foreignKeyID(tc.in); got != tc.want {
				t.Errorf("foreignKeyID(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
