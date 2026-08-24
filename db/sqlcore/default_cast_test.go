package sqlcore

// A SQL type this package has no literal rule for gets its DEFAULT rendered as a
// CAST, and a CAST is an expression. SQLite accepts an expression as a DEFAULT
// only in parentheses, so without them CREATE TABLE and ALTER TABLE ADD COLUMN
// are both syntax errors and the model cannot be created (audit N3).
//
// The behaviour is proved end to end against a real SQLite in
// tests/e2e/migrate_custom_sqltype_test.go. This pins the emitted SQL for both
// drivers, because the Postgres lane cannot be run here and it is the driver
// that accepted the broken form — so nothing else would notice it regressing.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"

	maniflex "github.com/xaleel/maniflex"
)

type castMoney struct{ Cents int64 }

func (castMoney) SQLType(maniflex.DriverType) string { return "NUMERIC(12,2)" }
func (m castMoney) Value() (driver.Value, error)     { return m.Cents, nil }
func (m *castMoney) Scan(any) error                  { return nil }

func castField(dbName, def string) maniflex.FieldMeta {
	f := maniflex.FieldMeta{Name: "Total", Type: reflect.TypeOf(castMoney{})}
	f.Tags.DBName = dbName
	f.Tags.JSONName = dbName
	f.Tags.Default = def
	return f
}

// recordingExec captures the DDL instead of running it, so the Postgres
// statement can be inspected on a machine with no Postgres.
type recordingExec struct{ stmts []string }

func (r *recordingExec) ExecContext(_ context.Context, q string, _ ...any) (sql.Result, error) {
	r.stmts = append(r.stmts, q)
	return driver.RowsAffected(0), nil
}
func (r *recordingExec) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}

func TestCastDefaultIsParenthesisedOnBothDrivers(t *testing.T) {
	for _, drv := range []maniflex.DriverType{maniflex.SQLite, maniflex.Postgres} {
		a := &Adapter{driver: drv}

		t.Run("create table/"+driverName(drv), func(t *testing.T) {
			def := a.columnDef(castField("total", ""))
			assertParenthesisedCast(t, def)
		})

		// The synthesised zero default and an explicit mfx:"default:" both render
		// through quotedDefault, and both reach ALTER TABLE ADD COLUMN — the
		// statement that runs against a table which already has rows.
		for _, tc := range []struct{ name, def string }{
			{"synthesised zero", ""},
			{"explicit default tag", "0"},
		} {
			t.Run("add column/"+driverName(drv)+"/"+tc.name, func(t *testing.T) {
				rec := &recordingExec{}
				if err := a.addColumn(context.Background(), rec, "invoices", castField("total", tc.def)); err != nil {
					t.Fatalf("addColumn: %v", err)
				}
				if len(rec.stmts) != 1 {
					t.Fatalf("issued %d statements, want 1: %v", len(rec.stmts), rec.stmts)
				}
				assertParenthesisedCast(t, rec.stmts[0])
			})
		}
	}
}

func assertParenthesisedCast(t *testing.T, sql string) {
	t.Helper()
	if !strings.Contains(sql, "CAST(") {
		t.Fatalf("no CAST in the statement, so this no longer covers what it was written for:\n%s", sql)
	}
	if !strings.Contains(sql, "DEFAULT (CAST(") {
		t.Errorf("the CAST default is not parenthesised, which SQLite rejects outright:\n%s", sql)
	}
}
