package e2e

// A column whose SQLTyper reports a SQL type the migrator has no literal rule
// for gets its zero-value DEFAULT rendered as a CAST — and a CAST is an
// expression, which SQLite accepts as a DEFAULT only in parentheses. Without
// them the statement is a syntax error and the model cannot be created at all
// (audit N3).
//
// Postgres accepts the unparenthesised form, so this failed only on SQLite: the
// model migrates in production and will not create locally, the reverse of the
// usual dev/prod split and the harder one to believe.
//
// Nothing shipped hits it, which is why no test did: LocaleString and the JSON
// containers all report TEXT on SQLite, which has a literal rule. It takes a
// money type — NUMERIC(12,2) — or a blob, both of which the zeroDefaultSQL
// comment names as the case it exists for.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// n3Money reports NUMERIC(12,2) — a SQL type with a precision, so it can never
// be on a fixed allowlist of type names.
type n3Money struct{ Cents int64 }

func (n3Money) SQLType(maniflex.DriverType) string { return "NUMERIC(12,2)" }
func (m n3Money) Value() (driver.Value, error)     { return m.Cents, nil }
func (m *n3Money) Scan(v any) error {
	if v == nil {
		return nil
	}
	if i, ok := v.(int64); ok {
		m.Cents = i
	}
	return nil
}

type n3Blob []byte

func (n3Blob) SQLType(d maniflex.DriverType) string {
	if d == maniflex.Postgres {
		return "BYTEA"
	}
	return "BLOB"
}
func (b n3Blob) Value() (driver.Value, error) { return []byte(b), nil }
func (b *n3Blob) Scan(v any) error {
	if raw, ok := v.([]byte); ok {
		*b = append(n3Blob(nil), raw...)
	}
	return nil
}

type n3Invoice struct {
	maniflex.BaseModel
	Ref      string  `json:"ref"`
	Total    n3Money `json:"total"`
	Discount n3Money `json:"discount" mfx:"default:0"` // the explicit-tag path renders through the same function
	Seal     n3Blob  `json:"seal"`
}

// n3InvoiceV1 is the same table before the custom-typed columns exist, so
// migrating to n3Invoice goes through ALTER TABLE ADD COLUMN rather than
// CREATE TABLE. Both build the DEFAULT clause the same way.
type n3InvoiceV1 struct {
	maniflex.BaseModel
	Ref string `json:"ref"`
}

func n3Config() maniflex.ModelConfig { return maniflex.ModelConfig{TableName: "n3_invoices"} }

func TestMigrateCreatesATableWithACustomSQLType(t *testing.T) {
	t.Parallel()
	db := n3Open(t, filepath.Join(t.TempDir(), "create.db"), n3Invoice{})

	// The row inserts without naming the custom columns, which is what the
	// synthesised DEFAULT is for: the columns are NOT NULL.
	res := db.Raw(context.Background(), `INSERT INTO n3_invoices (id, ref) VALUES (?, ?)`, "1", "a")
	if _, err := res.RowsAffected(); err != nil {
		t.Fatalf("insert without the custom columns: %v", err)
	}
}

func TestMigrateAddsACustomSQLTypeColumnToAnExistingTable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "alter.db")

	db := n3Open(t, path, n3InvoiceV1{})
	if _, err := db.Raw(context.Background(),
		`INSERT INTO n3_invoices (id, ref) VALUES (?, ?)`, "1", "a").RowsAffected(); err != nil {
		t.Fatalf("seed the pre-migration table: %v", err)
	}
	db.Close()

	// A NOT NULL column added to a table that already has rows needs the DEFAULT
	// to be valid, so this is the statement the bug actually broke.
	db2 := n3Open(t, path, n3Invoice{})
	var total any
	if err := n3ScanOne(db2, `SELECT total FROM n3_invoices WHERE id = '1'`, &total); err != nil {
		t.Fatalf("read the back-filled column: %v", err)
	}
	if fmt.Sprint(total) != "0" {
		t.Errorf("existing row got total=%v, want the zero the DEFAULT names", total)
	}
}

func n3Open(t *testing.T, path string, model any) maniflex.DBAdapter {
	t.Helper()
	srv := maniflex.New(maniflex.Config{})
	db, err := sqlite.Open(path, srv.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetDB(db)
	srv.MustRegister(model, n3Config())
	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func n3ScanOne(db maniflex.DBAdapter, query string, into ...any) error {
	rows, err := db.Raw(context.Background(), query).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("no rows")
	}
	if err := rows.Scan(into...); err != nil {
		return err
	}
	return rows.Err()
}
