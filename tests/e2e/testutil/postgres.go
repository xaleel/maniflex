package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"maniflex"
	"maniflex/db/postgres"
)

// ── Postgres lane DSN plumbing ─────────────────────────────────────────────────
//
// The DSN is supplied by the e2e TestMain — either from a testcontainers
// Postgres instance it starts, or from MANIFLEX_TEST_PG_DSN when an external
// server is already available. testutil itself never starts a container; it only
// consumes the DSN so the heavy testcontainers dependency stays in the e2e
// package's TestMain rather than this shared helper.

var (
	pgDSNMu sync.RWMutex
	pgDSN   string

	// One shared admin adapter (default search_path) does CREATE/DROP SCHEMA for
	// every per-test schema, so we don't open a fresh admin pool per test.
	pgAdminOnce sync.Once
	pgAdmin     maniflex.DBAdapter
	pgAdminErr  error
)

// SetPostgresDSN records the DSN the Postgres lane connects to. Call it from the
// e2e TestMain once the container (or override DSN) is ready, before m.Run().
func SetPostgresDSN(dsn string) {
	pgDSNMu.Lock()
	defer pgDSNMu.Unlock()
	pgDSN = dsn
}

func postgresDSN() string {
	pgDSNMu.RLock()
	defer pgDSNMu.RUnlock()
	return pgDSN
}

// PostgresDSN returns the DSN the Postgres lane connects to, or "" when none is
// configured. Exported so tests that exercise the connection/provisioning path
// itself (e.g. schema auto-creation) can open their own adapter outside the
// per-test schema helper, which pre-creates the schema.
func PostgresDSN() string { return postgresDSN() }

func publicSchema() string { return "public" }

// adminAdapter lazily opens the shared schema-management adapter scoped to the
// public schema.
func adminAdapter() (maniflex.DBAdapter, error) {
	pgAdminOnce.Do(func() {
		dsn := postgresDSN()
		if dsn == "" {
			pgAdminErr = fmt.Errorf("Postgres lane selected but no DSN configured")
			return
		}
		pub := publicSchema()
		pgAdmin, pgAdminErr = postgres.OpenWithConfig(dsn, "", &maniflex.Registry{},
			postgres.PoolConfig{MaxOpenConns: 4}, postgres.PoolConfig{MaxOpenConns: 4},
			postgres.SessionConfig{SchemaName: &pub})
	})
	return pgAdmin, pgAdminErr
}

func randSchema() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "mfxt_" + hex.EncodeToString(b[:])
}

// openPostgres provisions an isolated, per-test Postgres schema and returns a
// maniflex adapter scoped to it via search_path. The schema is created before
// the scoped adapter opens (so AutoMigrate's DDL lands inside it) and DROPped in
// t.Cleanup — preserving the clean-slate, t.Parallel()-safe semantics the
// in-memory SQLite lane gives today.
//
// search_path is applied at the connection level (SessionConfig.SchemaName is
// re-issued in every pooled connection's Connect hook), not via a one-shot SET,
// so every connection in the scoped pool lands in the right schema.
func openPostgres(t testing.TB, reg maniflex.RegistryAccessor) maniflex.DBAdapter {
	t.Helper()

	admin, err := adminAdapter()
	if err != nil {
		t.Fatalf("testutil: %v. Run the Postgres lane via the e2e TestMain "+
			"(testcontainers) or set MANIFLEX_TEST_PG_DSN.", err)
	}

	schema := randSchema()
	ctx := context.Background()
	if _, err := admin.Raw(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)).RowsAffected(); err != nil {
		t.Fatalf("testutil: create schema %s: %v", schema, err)
	}

	dropSchema := func() {
		if _, err := admin.Raw(context.Background(),
			fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)).RowsAffected(); err != nil {
			t.Logf("testutil: drop schema %s: %v", schema, err)
		}
	}

	// Small pools per test bound total connection use under -p 8: each test holds
	// at most a handful of connections, keeping the suite well under a default
	// Postgres max_connections while many tests run in parallel.
	scoped, err := postgres.OpenWithConfig(postgresDSN(), "", reg,
		postgres.PoolConfig{MaxOpenConns: 4}, postgres.PoolConfig{MaxOpenConns: 4},
		postgres.SessionConfig{SchemaName: &schema})
	if err != nil {
		dropSchema()
		t.Fatalf("testutil: open postgres (schema %s): %v", schema, err)
	}

	t.Cleanup(func() {
		scoped.Close()
		dropSchema()
	})

	return scoped
}
