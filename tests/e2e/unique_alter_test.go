package e2e

// Regression (notifications svc #10): mfx:"unique" was honoured only when
// AutoMigrate CREATEd the table. When the column was added later via
// ALTER TABLE ADD COLUMN onto an existing table, the UNIQUE was silently skipped
// (warn only), so a uniqueness constraint correctness depended on vanished on the
// add-column redeploy. AutoMigrate now creates a UNIQUE INDEX for the added
// column, and fails loudly when existing rows already violate it.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// itemV1 is the "before" schema: no unique code column.
type itemV1 struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// itemV2 adds a unique code column onto the same table.
type itemV2 struct {
	maniflex.BaseModel
	Name string `json:"name"`
	Code string `json:"code" mfx:"unique"`
}

const alterItemsTable = "alter_items"

// migrateV1 opens the file DB, registers itemV1 on the shared table, migrates,
// and seeds `rows` rows. Returns after closing so a second adapter can reopen.
func migrateV1AndSeed(t *testing.T, dbPath string, rows int) {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	db, err := sqlite.Open(dbPath, srv.Registry())
	if err != nil {
		t.Fatalf("open v1: %v", err)
	}
	defer db.Close()
	srv.SetDB(db)
	srv.MustRegister(itemV1{}, maniflex.ModelConfig{TableName: alterItemsTable})
	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("migrate v1: %v", err)
	}
	for i := 0; i < rows; i++ {
		res := db.Raw(context.Background(),
			"INSERT INTO "+alterItemsTable+" (id, name) VALUES (?, ?)",
			"id-"+string(rune('a'+i)), "row")
		if _, err := res.RowsAffected(); err != nil {
			t.Fatalf("seed v1 row %d: %v", i, err)
		}
	}
}

// migrateV2 reopens the same file DB and migrates itemV2 (which adds the unique
// column). Returns the still-open adapter and the migration error (nil on success).
func migrateV2(t *testing.T, dbPath string) (maniflex.DBAdapter, error) {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	db, err := sqlite.Open(dbPath, srv.Registry())
	if err != nil {
		t.Fatalf("open v2: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetDB(db)
	srv.MustRegister(itemV2{}, maniflex.ModelConfig{TableName: alterItemsTable})
	return db, srv.MigrateOnly(context.Background())
}

func TestUniqueOnAlter_EnforcedOnCleanData(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "clean.db")

	// One existing row → adding the unique column is fine.
	migrateV1AndSeed(t, dbPath, 1)

	db, err := migrateV2(t, dbPath)
	if err != nil {
		t.Fatalf("migrate v2 on clean data should succeed, got: %v", err)
	}

	// The unique index must now be enforced: two rows with the same code fail.
	first := db.Raw(context.Background(),
		"INSERT INTO "+alterItemsTable+" (id, name, code) VALUES (?, ?, ?)", "x1", "n", "DUP")
	if _, err := first.RowsAffected(); err != nil {
		t.Fatalf("first code insert: %v", err)
	}
	second := db.Raw(context.Background(),
		"INSERT INTO "+alterItemsTable+" (id, name, code) VALUES (?, ?, ?)", "x2", "n", "DUP")
	if _, err := second.RowsAffected(); err == nil {
		t.Fatal("duplicate code insert must be rejected by the UNIQUE index, but it succeeded")
	}
}

func TestUniqueOnAlter_FailsBootOnDuplicateData(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "dirty.db")

	// Two existing rows → both receive the same zero-default for the new column,
	// so the UNIQUE index cannot be built. Migration must fail loudly.
	migrateV1AndSeed(t, dbPath, 2)

	_, err := migrateV2(t, dbPath)
	if err == nil {
		t.Fatal("migrate v2 onto duplicate data must fail, got nil")
	}
	if !strings.Contains(err.Error(), alterItemsTable) || !strings.Contains(strings.ToLower(err.Error()), "code") {
		t.Fatalf("error should name the table and column, got: %v", err)
	}
}
