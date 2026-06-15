package e2e

// postgres_schema_test.go covers roadmap §10.5: the Postgres adapter provisions
// its configured search_path schema on connect instead of leaving SET search_path
// pointing at a non-existent schema.
//
// testutil.openPostgres pre-creates each per-test schema, so the regular lane
// never exercises the auto-create path. This test deliberately opens its own
// adapter against a schema that has never been created, asserting that:
//   - Open creates the schema on connect (verified against pg_catalog), and
//   - AutoMigrate's CREATE TABLE then lands inside that schema.
//
// Before §10.5, the same flow failed: SET search_path silently accepted the
// missing schema and the first CREATE TABLE errored with 3F000 / 42P03
// ("no schema has been selected to create in").

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/postgres"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestPostgresSchemaAutoCreate(t *testing.T) {
	if !testutil.IsPostgres() {
		t.Skip("schema auto-create is Postgres-specific")
	}
	dsn := testutil.PostgresDSN()
	if dsn == "" {
		t.Skip("no Postgres DSN configured")
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "mfx_autocreate_" + hex.EncodeToString(b[:])

	srv := maniflex.New(maniflex.Config{AutoMigrate: false})
	srv.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})

	db, err := postgres.OpenWithConfig(dsn, "", srv.Registry(),
		postgres.PoolConfig{MaxOpenConns: 2}, postgres.PoolConfig{MaxOpenConns: 2},
		postgres.SessionConfig{SchemaName: &schema})
	if err != nil {
		t.Fatalf("open postgres against fresh schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := db.Raw(context.Background(),
			"DROP SCHEMA IF EXISTS "+schema+" CASCADE").RowsAffected(); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		db.Close()
	})

	ctx := context.Background()

	// The schema must exist immediately after Open — created on connect, before
	// any migration runs.
	if !schemaExists(t, db, schema) {
		t.Fatalf("schema %s was not created on connect", schema)
	}

	// AutoMigrate must now succeed: CREATE TABLE lands inside the new schema.
	if err := db.AutoMigrate(ctx, srv.Registry()); err != nil {
		t.Fatalf("auto-migrate into auto-created schema: %v", err)
	}

	// And the table must live in the auto-created schema, not leak into public.
	if !tableExistsInSchema(t, db, schema, "products") {
		t.Fatalf("products table not found in schema %s after AutoMigrate", schema)
	}
}

func schemaExists(t *testing.T, db maniflex.DBAdapter, schema string) bool {
	t.Helper()
	rows, err := db.Raw(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		schema).Rows()
	if err != nil {
		t.Fatalf("query pg_namespace: %v", err)
	}
	defer rows.Close()
	var exists bool
	if rows.Next() {
		if err := rows.Scan(&exists); err != nil {
			t.Fatalf("scan schema exists: %v", err)
		}
	}
	return exists
}

func tableExistsInSchema(t *testing.T, db maniflex.DBAdapter, schema, table string) bool {
	t.Helper()
	rows, err := db.Raw(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		   WHERE table_schema = $1 AND table_name = $2)`,
		schema, table).Rows()
	if err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	defer rows.Close()
	var exists bool
	if rows.Next() {
		if err := rows.Scan(&exists); err != nil {
			t.Fatalf("scan table exists: %v", err)
		}
	}
	return exists
}
