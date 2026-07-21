package sql

// Audit JB-6: New auto-detected the SQL dialect with
// strings.Contains(reflect.TypeOf(db.Driver()).String(), "pq"|"postgres").
// The jackc/pgx driver registers as package "stdlib", type "stdlib.Driver" —
// which contains neither token — so a Postgres database driven by pgx was read
// as SQLite. That is not a slow path: the dialect picks both the SQL (Postgres
// needs FOR UPDATE SKIP LOCKED) and the placeholder style ($1 vs ?), so every
// query then fails against Postgres.
//
// Detection now classifies by the driver's package path, which is stable across
// versions. These tests cover the classifier and the WithDriver override
// directly. The exact reflect strings pgx produces are asserted from its known
// module path rather than by importing pgx (which would weigh down this lean
// module); the match is on the "jackc/pgx" module-path substring, which is
// definitional. Real modernc-sqlite detection is exercised end-to-end by every
// jobs_sql test, which construct New(sqliteDB) and round-trip a job.
//
//	go test ./jobs/sql/ -run TestDriver

import "testing"

func TestDriver_ClassifiesByPackagePath(t *testing.T) {
	cases := []struct {
		name    string
		pkgPath string
		typ     string
		wantPG  bool
	}{
		{"pgx v5", "github.com/jackc/pgx/v5/stdlib", "stdlib.Driver", true},
		{"pgx v4", "github.com/jackc/pgx/v4/stdlib", "stdlib.Driver", true},
		{"lib/pq", "github.com/lib/pq", "pq.Driver", true},
		{"cockroach pgx", "github.com/cockroachdb/cockroach-go/crdb", "crdb.Driver", true},
		{"modernc sqlite", "modernc.org/sqlite", "sqlite.Driver", false},
		{"mattn sqlite3", "github.com/mattn/go-sqlite3", "sqlite3.SQLiteDriver", false},
		// Fallback: a Postgres driver not matched by path is still caught by the
		// old name heuristic, so pre-existing setups do not regress.
		{"unknown pq by name", "", "pq.Driver", true},
		{"unknown postgres by name", "", "somepostgres.Driver", true},
		// A SQLite driver must never trip the fallback.
		{"unknown sqlite by name", "", "sqlite.Driver", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPostgresDriver(c.pkgPath, c.typ); got != c.wantPG {
				t.Errorf("isPostgresDriver(%q, %q) = %v, want %v", c.pkgPath, c.typ, got, c.wantPG)
			}
		})
	}
}

// The regression in one line: the specific pgx identity the old code missed.
func TestDriver_PgxIsNotMistakenForSQLite(t *testing.T) {
	if !isPostgresDriver("github.com/jackc/pgx/v5/stdlib", "stdlib.Driver") {
		t.Fatal("pgx is classified as SQLite — every query would fail against Postgres")
	}
}

// WithDriver is authoritative and does not consult the driver, so it works even
// when detection could not (a nil db here would panic if detection ran).
func TestDriver_WithDriverOverrides(t *testing.T) {
	cases := []struct {
		explicit string
		wantPG   bool
	}{
		{"postgres", true},
		{"postgresql", true},
		{"pgx", true},
		{"sqlite", false},
		{"sqlite3", false},
		{"  Postgres  ", true}, // trimmed and case-folded
	}
	for _, c := range cases {
		if got := resolveIsPG(c.explicit, nil); got != c.wantPG {
			t.Errorf("resolveIsPG(%q) = %v, want %v", c.explicit, got, c.wantPG)
		}
	}
}

// A blank driver must fall through to detection — asserted here only that the
// explicit branch does not swallow it (the detection call itself needs a db and
// is covered end-to-end elsewhere).
func TestDriver_EmptyDriverFallsThroughToDetection(t *testing.T) {
	defer func() {
		// resolveIsPG("", nil) reaches detectPostgres(nil) → db.Driver() panics.
		// The panic is the proof that "" did NOT take the explicit branch.
		if recover() == nil {
			t.Error(`resolveIsPG("", nil) did not fall through to detection`)
		}
	}()
	resolveIsPG("", nil)
}
