package e2e

import (
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// skipRawSQLOnPostgres skips a test whose body hand-writes raw SQL with `?`
// placeholders (the SQLite dialect). Raw SQL is driver-specific by design — the
// raw-query escape hatch hands the caller's SQL straight to the driver, which
// on Postgres requires `$N` placeholders — so these tests exercise SQLite
// dialect text and are not meaningful on the Postgres lane.
func skipRawSQLOnPostgres(t *testing.T) {
	t.Helper()
	if testutil.IsPostgres() {
		t.Skip("raw SQL uses `?` placeholders (SQLite dialect); not portable to Postgres")
	}
}

// skipSQLiteFileDBOnPostgres skips a test that drives an on-disk SQLite database
// directly (tempDB + openAdapter helpers, shared via Options.DBPath across
// multiple servers). The Postgres lane gives every NewServer its own isolated
// schema and ignores DBPath, so these cross-version, shared-file migration
// scenarios are inherently SQLite-only.
func skipSQLiteFileDBOnPostgres(t *testing.T) {
	t.Helper()
	if testutil.IsPostgres() {
		t.Skip("drives an on-disk SQLite database via DBPath; Postgres lane isolates per-schema")
	}
}
