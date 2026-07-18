package e2e

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	_ "modernc.org/sqlite"
)

// Roadmap 10.3: a UNIQUE index that could not be created is a broken invariant,
// not a slow query — the model declares a constraint the database is not
// enforcing, and every write that should have been refused is silently accepted.
//
//	go test ./tests/e2e/... -run TestUniqueIndex

// uiDoc declares a unique index on slug through ModelConfig.
type uiDoc struct {
	maniflex.BaseModel
	Slug string `json:"slug" db:"slug"`
}

// uiPlainDoc is the fixture for the plain-vs-unique severity split.
type uiPlainDoc struct {
	maniflex.BaseModel
	Slug string `json:"slug" db:"slug"`
}

// uiSecret has an encrypted+unique field, which the migrator backs with a
// generated <col>_hmac column and a UNIQUE index over it.
type uiSecret struct {
	maniflex.BaseModel
	Email string `json:"email" db:"email" mfx:"encrypted,unique"`
}

// migrateInto registers models against a fresh adapter on dbPath and runs
// AutoMigrate, returning its error. Driving the adapter directly keeps the test
// on the migration rather than on booting a listener.
func migrateInto(t *testing.T, dbPath string, models ...any) error {
	t.Helper()
	srv := maniflex.New(maniflex.Config{Port: 0})
	if err := srv.Register(models...); err != nil {
		t.Fatalf("Register: %v", err)
	}
	adapter, err := sqlite.Open(dbPath, srv.Registry())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := adapter.(interface{ Close() error }); ok {
			c.Close()
		}
	})
	return adapter.AutoMigrate(context.Background(), srv.Registry())
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	return n > 0
}

// A table already holding duplicates cannot take the unique index the model
// declares, and that must stop the migration rather than be logged past.
func TestUniqueIndex_DuplicateDataFailsMigration(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "dup.db")
	db := rawDB(t, dbPath)

	mustExec(t, db, `CREATE TABLE ui_docs (id TEXT PRIMARY KEY, slug TEXT)`)
	mustExec(t, db, `INSERT INTO ui_docs (id, slug) VALUES ('a', 'same')`)
	mustExec(t, db, `INSERT INTO ui_docs (id, slug) VALUES ('b', 'same')`)

	err := migrateInto(t, dbPath, uiDoc{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{
			{Name: "uidx_ui_docs_slug", Columns: []string{"slug"}, Unique: true},
		},
	})
	if err == nil {
		t.Fatal("migration must fail: the unique index the model declares cannot exist over this data")
	}
	msg := err.Error()
	for _, want := range []string{"uidx_ui_docs_slug", "ui_docs", "slug", "de-duplicate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should name %q so the operator can act on it: %s", want, msg)
		}
	}
}

// The same declaration over clean data migrates and the constraint is real.
func TestUniqueIndex_CleanDataMigratesAndEnforces(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "clean.db")

	err := migrateInto(t, dbPath, uiDoc{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{
			{Name: "uidx_ui_docs_slug", Columns: []string{"slug"}, Unique: true},
		},
	})
	if err != nil {
		t.Fatalf("migration over clean data must succeed: %v", err)
	}

	db := rawDB(t, dbPath)
	if !indexExists(t, db, "uidx_ui_docs_slug") {
		t.Fatal("the unique index was not created")
	}
	// And it is enforcing, not merely present.
	mustExec(t, db, `INSERT INTO ui_docs (id, slug) VALUES ('a', 'same')`)
	if _, err := db.Exec(`INSERT INTO ui_docs (id, slug) VALUES ('b', 'same')`); err == nil {
		t.Error("the index exists but is not enforcing uniqueness")
	}
}

// blockIndexName occupies an index name with a table, so CREATE INDEX under
// that name fails. It is a way to force a creation failure that does not depend
// on the data — note that a bad *column* name will not do it, because SQLite
// resolves an unmatched double-quoted identifier to a string literal and
// cheerfully indexes the constant.
func blockIndexName(t *testing.T, dbPath, name string) {
	t.Helper()
	mustExec(t, rawDB(t, dbPath), `CREATE TABLE `+name+` (x TEXT)`)
}

// The severity split, proven on one identical cause: the same failure is
// tolerated for a plain index and fatal for a unique one.
//
// A missing plain index costs a table scan, which the application survives. A
// missing unique index means the model declares a constraint the database is
// not enforcing, and duplicates are accepted silently from then on.
func TestUniqueIndex_PlainIndexFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "plain.db")
	blockIndexName(t, dbPath, "idx_blocked")

	err := migrateInto(t, dbPath, uiPlainDoc{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{
			{Name: "idx_blocked", Columns: []string{"slug"}},
		},
	})
	if err != nil {
		t.Fatalf("a failed plain index must not fail the migration: %v", err)
	}
	if indexExists(t, rawDB(t, dbPath), "idx_blocked") {
		t.Fatal("precondition broken: the index was created, so nothing failed and " +
			"this test proves nothing")
	}
}

func TestUniqueIndex_SameFailureOnUniqueIndexIsFatal(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "blocked-unique.db")
	blockIndexName(t, dbPath, "uidx_blocked")

	err := migrateInto(t, dbPath, uiPlainDoc{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{
			{Name: "uidx_blocked", Columns: []string{"slug"}, Unique: true},
		},
	})
	if err == nil {
		t.Fatal("a unique index that could not be created must fail the migration")
	}
	if !strings.Contains(err.Error(), "uidx_blocked") {
		t.Errorf("error should name the index: %v", err)
	}
}

// An encrypted+unique field gets a generated <col>_hmac column and a UNIQUE
// index over it.
func TestUniqueIndex_HMACIndexIsCreated(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "hmac.db")

	if err := migrateInto(t, dbPath, uiSecret{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !indexExists(t, rawDB(t, dbPath), "uidx_ui_secrets_email_hmac") {
		t.Fatal("the HMAC unique index was not created")
	}
}

// The retry gap: the index creation used to sit inside the "hmac column is
// missing" branch, so a boot that added the column but failed to build the
// index never tried again — every later boot saw the column, skipped the block,
// and left the constraint permanently absent.
func TestUniqueIndex_HMACIndexIsRetriedWhenColumnAlreadyExists(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "retry.db")
	db := rawDB(t, dbPath)

	// Arrange the exact state that boot would leave behind: the column present,
	// the index absent.
	mustExec(t, db, `CREATE TABLE ui_secrets (
		id TEXT PRIMARY KEY, email TEXT, email_hmac TEXT NOT NULL DEFAULT '')`)
	if indexExists(t, db, "uidx_ui_secrets_email_hmac") {
		t.Fatal("precondition: the index must not exist yet")
	}

	if err := migrateInto(t, dbPath, uiSecret{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if !indexExists(t, db, "uidx_ui_secrets_email_hmac") {
		t.Error("the missing HMAC index was not created — a constraint the model " +
			"declares would stay absent for the life of the deployment")
	}
}

// And the retry surfaces data that already violates the constraint rather than
// quietly leaving it unenforced.
func TestUniqueIndex_HMACRetrySurfacesExistingDuplicates(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "retry-dup.db")
	db := rawDB(t, dbPath)

	mustExec(t, db, `CREATE TABLE ui_secrets (
		id TEXT PRIMARY KEY, email TEXT, email_hmac TEXT NOT NULL DEFAULT '')`)
	mustExec(t, db, `INSERT INTO ui_secrets (id, email, email_hmac) VALUES ('a', 'x', 'dup')`)
	mustExec(t, db, `INSERT INTO ui_secrets (id, email, email_hmac) VALUES ('b', 'y', 'dup')`)

	err := migrateInto(t, dbPath, uiSecret{})
	if err == nil {
		t.Fatal("duplicate values in an encrypted+unique field must fail the migration")
	}
	if !strings.Contains(err.Error(), "de-duplicate") {
		t.Errorf("error should name the remedy: %v", err)
	}
}
