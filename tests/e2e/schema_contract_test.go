package e2e

// SC-1 — the column contract, pinned against the DDL AutoMigrate actually
// emits.
//
// This exists because the encoding is load-bearing for anything reading the
// database from outside — a migration tool, a reporting replica, BI, another
// service. maniflex maps a non-pointer field to a NOT NULL column and stores
// the zero value rather than NULL for "absent", so NOT NULL on its own says
// nothing about whether a value is required; the only schema-level signal is
// whether a DEFAULT clause is attached. Reading NOT NULL as "required" once
// cost a real migration every storefront order in the database.
//
// The contract is documented in docs/src/defining-your-api/schema.md. These
// tests are what stop the document and the DDL drifting apart.

import (
	"context"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// SchemaSpec exercises every shape the column contract distinguishes.
type SchemaSpec struct {
	maniflex.BaseModel

	// Required scalars: NOT NULL, and deliberately no DEFAULT.
	ReqText string `json:"reqText" db:"req_text" mfx:"required"`
	ReqNum  int    `json:"reqNum"  db:"req_num"  mfx:"required"`

	// Optional non-pointer scalars: NOT NULL with a zero DEFAULT. The zero
	// value, not NULL, is what "absent" means for these.
	OptText string  `json:"optText" db:"opt_text"`
	OptNum  int     `json:"optNum"  db:"opt_num"`
	OptRate float64 `json:"optRate" db:"opt_rate"`
	OptFlag bool    `json:"optFlag" db:"opt_flag"`

	// Pointer scalars: genuinely nullable. This is how a field says "may have
	// no value" to the database.
	NullText *string `json:"nullText" db:"null_text"`
	NullNum  *int    `json:"nullNum"  db:"null_num"`

	// An explicit default applies whether or not the column is nullable.
	Tagged string `json:"tagged" db:"tagged" mfx:"default:pending"`
}

// tableDDL returns the CREATE TABLE statement SQLite stored for the table.
func tableDDL(t *testing.T, srv *testutil.Server, table string) string {
	t.Helper()
	bg := maniflex.NewBackground(context.Background(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
	rows, err := bg.RawQuery(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table)
	if err != nil {
		t.Fatalf("introspect schema: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one table named %q, got %d rows", table, len(rows))
	}
	ddl, _ := rows[0]["sql"].(string)
	return ddl
}

// columnDDL pulls one column's definition out of a CREATE TABLE statement.
func columnDDL(t *testing.T, ddl, column string) string {
	t.Helper()
	for _, part := range strings.Split(ddl, ",") {
		part = strings.TrimSpace(strings.Trim(part, "()\n\t "))
		if strings.HasPrefix(part, `"`+column+`" `) {
			return part
		}
	}
	t.Fatalf("column %q not found in DDL:\n%s", column, ddl)
	return ""
}

func schemaSpecDDL(t *testing.T) string {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SchemaSpec{}}})
	return tableDDL(t, srv, "schema_specs")
}

// A required scalar is NOT NULL with NO default. The absent DEFAULT is the only
// thing distinguishing it from an optional one at the schema level.
func TestSchemaContract_RequiredScalarIsNotNullWithoutDefault(t *testing.T) {
	skipRawSQLOnPostgres(t)
	t.Parallel()
	ddl := schemaSpecDDL(t)

	for _, col := range []string{"req_text", "req_num"} {
		def := columnDDL(t, ddl, col)
		if !strings.Contains(def, "NOT NULL") {
			t.Errorf("%s: want NOT NULL, got %q", col, def)
		}
		if strings.Contains(def, "DEFAULT") {
			t.Errorf("%s: a required column must carry no DEFAULT, got %q", col, def)
		}
	}
}

// An optional non-pointer scalar is ALSO NOT NULL — it simply carries a zero
// default. This is the pair that makes NOT NULL uninformative on its own.
func TestSchemaContract_OptionalScalarIsNotNullWithZeroDefault(t *testing.T) {
	skipRawSQLOnPostgres(t)
	t.Parallel()
	ddl := schemaSpecDDL(t)

	for col, zero := range map[string]string{
		"opt_text": `DEFAULT ''`,
		"opt_num":  "DEFAULT 0",
		"opt_rate": "DEFAULT 0",
		"opt_flag": "DEFAULT 0",
	} {
		def := columnDDL(t, ddl, col)
		if !strings.Contains(def, "NOT NULL") {
			t.Errorf("%s: want NOT NULL, got %q", col, def)
		}
		if !strings.Contains(def, zero) {
			t.Errorf("%s: want %s, got %q", col, zero, def)
		}
	}
}

// A pointer is how a field becomes genuinely nullable.
func TestSchemaContract_PointerScalarIsNullable(t *testing.T) {
	skipRawSQLOnPostgres(t)
	t.Parallel()
	ddl := schemaSpecDDL(t)

	for _, col := range []string{"null_text", "null_num"} {
		def := columnDDL(t, ddl, col)
		if strings.Contains(def, "NOT NULL") {
			t.Errorf("%s: a pointer field must be nullable, got %q", col, def)
		}
		if strings.Contains(def, "DEFAULT") {
			t.Errorf("%s: a nullable column gets no synthesised default, got %q", col, def)
		}
	}
}

func TestSchemaContract_ExplicitDefaultIsEmitted(t *testing.T) {
	skipRawSQLOnPostgres(t)
	t.Parallel()
	def := columnDDL(t, schemaSpecDDL(t), "tagged")
	if !strings.Contains(def, `DEFAULT 'pending'`) {
		t.Errorf("tagged: want DEFAULT 'pending', got %q", def)
	}
}

// The consequence the contract exists to state: an omitted optional string
// reads back as "" and not as NULL, so "no reference" is the zero value.
func TestSchemaContract_OmittedOptionalReadsBackAsZeroNotNull(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SchemaSpec{}}})

	created := srv.POST("/schema_specs", map[string]any{
		"reqText": "x", "reqNum": 1,
	})
	created.AssertStatus(201)

	row := created.Data()
	if got, ok := row["optText"]; !ok || got != "" {
		t.Errorf(`optText = %#v (present=%v), want "" — the zero value, not null`, got, ok)
	}
	if got, ok := row["optNum"]; !ok || got != float64(0) {
		t.Errorf("optNum = %#v (present=%v), want 0", got, ok)
	}
	// A pointer field is the one that genuinely comes back null.
	if got, ok := row["nullText"]; ok && got != nil {
		t.Errorf("nullText = %#v, want null", got)
	}
}
