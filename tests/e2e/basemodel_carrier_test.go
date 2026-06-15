package e2e

// Phase 1 / T1.1 PoD (lane-checked): AutoMigrate must create no columns for the
// unexported BaseModel carriers. Runs on both the SQLite and Postgres lanes.

import (
	"context"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

type carrierMigrateModel struct {
	maniflex.BaseModel
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestBaseModelCarriers_NoExtraColumns(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{carrierMigrateModel{}}})
	adapter := srv.ManiflexServer().DB()
	meta, ok := srv.ManiflexServer().Registry().Get("carrierMigrateModel")
	if !ok {
		t.Fatal("carrierMigrateModel not registered")
	}

	rows, err := adapter.Raw(context.Background(), "SELECT * FROM "+meta.TableName).Rows()
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}

	got := map[string]bool{}
	for _, c := range cols {
		got[c] = true
	}
	for _, leaked := range []string{"present", "extra", "select_fn", "selectFn"} {
		if got[leaked] {
			t.Errorf("carrier column %q was created by AutoMigrate; columns=%v", leaked, cols)
		}
	}
	want := []string{"id", "created_at", "updated_at", "name", "age"}
	if len(cols) != len(want) {
		t.Errorf("column count = %d (%v), want %d (%v)", len(cols), cols, len(want), want)
	}
}
