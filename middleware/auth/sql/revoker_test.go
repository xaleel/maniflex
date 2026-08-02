package sql

// AU-3 — the dialect half of the SQL-backed Revoker.
//
// The functional behaviour is exercised end-to-end in tests/e2e against a real
// database, on whichever lane is running. These are the assertions that cannot be
// made there: this module imports no driver, and the local lane is SQLite, so the
// Postgres statements are checked as the strings they are. That is the only place
// the two dialects diverge, and it is where the interesting bug lives — SQLite's
// max() and Postgres's GREATEST() disagree about NULL, which is a meaningful
// value in these columns rather than a missing one.
//
//	go test ./middleware/auth/sql/

import (
	"strings"
	"testing"
	"time"
)

func pgStatements(t *testing.T, opts ...Option) statements {
	t.Helper()
	c, err := newConfig(opts)
	if err != nil {
		t.Fatalf("newConfig: %v", err)
	}
	return buildStatements(c, true)
}

func sqliteStatements(t *testing.T, opts ...Option) statements {
	t.Helper()
	c, err := newConfig(opts)
	if err != nil {
		t.Fatalf("newConfig: %v", err)
	}
	return buildStatements(c, false)
}

func (s statements) all() map[string]string {
	return map[string]string{
		"revokeToken":    s.revokeToken,
		"isTokenRevoked": s.isTokenRevoked,
		"revokeUser":     s.revokeUser,
		"userCutoff":     s.userCutoff,
		"pruneTokens":    s.pruneTokens,
		"pruneUsers":     s.pruneUsers,
	}
}

// Postgres rejects "?" outright, so a statement that keeps one is not a subtly
// wrong query but a syntax error on every call.
func TestStatements_PostgresUsesNumberedPlaceholders(t *testing.T) {
	for name, q := range pgStatements(t).all() {
		if strings.Contains(q, "?") {
			t.Errorf("%s still carries a ? placeholder:\n%s", name, q)
		}
		if !strings.Contains(q, "$1") {
			t.Errorf("%s has no $1 placeholder:\n%s", name, q)
		}
	}
	// The two-argument statements must number both, and in order.
	if q := pgStatements(t).isTokenRevoked; !strings.Contains(q, "$1") || !strings.Contains(q, "$2") {
		t.Errorf("isTokenRevoked must bind $1 and $2:\n%s", q)
	}
	if q := pgStatements(t).revokeUser; !strings.Contains(q, "$3") {
		t.Errorf("revokeUser binds three values, so it must reach $3:\n%s", q)
	}
}

func TestStatements_SQLiteUsesQuestionMarks(t *testing.T) {
	for name, q := range sqliteStatements(t).all() {
		if strings.Contains(q, "$1") {
			t.Errorf("%s carries a Postgres placeholder:\n%s", name, q)
		}
	}
}

// The divergence this package exists to get right. NULL means "never drop", so
// the maximum of a NULL and a value must be NULL — which is what SQLite's max()
// does and the opposite of what Postgres's GREATEST() does. Both dialects must
// therefore wrap it in the CASE, or the Postgres deployment silently drops an
// entry that was meant to be permanent.
func TestStatements_NullDeadlineSurvivesTheGreatest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stmts    statements
		greatest string
	}{
		{"postgres", pgStatements(t), "GREATEST"},
		{"sqlite", sqliteStatements(t), "max("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.stmts.revokeToken, tc.greatest) {
				t.Errorf("revokeToken does not use %s:\n%s", tc.greatest, tc.stmts.revokeToken)
			}
			// Both must guard it, not just the dialect whose function is wrong.
			for _, q := range []string{tc.stmts.revokeToken, tc.stmts.revokeUser} {
				if !strings.Contains(q, "IS NULL") || !strings.Contains(q, "THEN NULL") {
					t.Errorf("the NULL guard is missing — a permanent entry can expire:\n%s", q)
				}
			}
		})
	}
}

// A read must not depend on the sweep having run, or an entry lingers past its
// deadline on any deployment where pruning is behind.
func TestStatements_ReadsFilterOnTheDeadline(t *testing.T) {
	s := sqliteStatements(t)
	if !strings.Contains(s.isTokenRevoked, `"expires_at" IS NULL OR "expires_at" >= ?`) {
		t.Errorf("isTokenRevoked does not filter on the deadline:\n%s", s.isTokenRevoked)
	}
	if !strings.Contains(s.userCutoff, `"retain_until" IS NULL OR "retain_until" >= ?`) {
		t.Errorf("userCutoff does not filter on the deadline:\n%s", s.userCutoff)
	}
}

// The sweep must be strictly narrower than the read, or it deletes a row the read
// would still have honoured — un-revoking a token a second early.
func TestStatements_PruneNeverDeletesAnHonouredRow(t *testing.T) {
	s := sqliteStatements(t)
	for name, q := range map[string]string{"pruneTokens": s.pruneTokens, "pruneUsers": s.pruneUsers} {
		if !strings.Contains(q, "< ?") {
			t.Errorf("%s must delete strictly below now, not at it:\n%s", name, q)
		}
		if !strings.Contains(q, "IS NOT NULL") {
			t.Errorf("%s must skip NULL deadlines — those never expire:\n%s", name, q)
		}
	}
}

func TestStatements_TablePrefixReachesEveryStatement(t *testing.T) {
	for name, q := range sqliteStatements(t, WithTablePrefix("auth_")).all() {
		if !strings.Contains(q, "auth_revoked_") {
			t.Errorf("%s ignores the table prefix:\n%s", name, q)
		}
		// "revoked_token" without the prefix would mean the default table is still
		// referenced somewhere in the statement.
		if strings.Contains(strings.ReplaceAll(q, "auth_revoked_", ""), "revoked_") {
			t.Errorf("%s still references an unprefixed table:\n%s", name, q)
		}
	}
}

// The name is interpolated into DDL and into derived index names, neither of
// which can bind a parameter, so a name outside the identifier grammar has to be
// refused rather than escaped.
func TestConfig_RejectsUnsafeTablePrefix(t *testing.T) {
	for _, bad := range []string{`x"; DROP TABLE users; --`, "a b", "a-b", "1x", `a"b`} {
		if _, err := newConfig([]Option{WithTablePrefix(bad)}); err == nil {
			t.Errorf("prefix %q was accepted", bad)
		}
	}
	for _, ok := range []string{"", "auth_", "myapp_", "A_"} {
		if _, err := newConfig([]Option{WithTablePrefix(ok)}); err != nil {
			t.Errorf("prefix %q was rejected: %v", ok, err)
		}
	}
}

// Rounding is toward refusing for longer, in both directions. A deadline that
// rounded down would stop being honoured before the credential it blocks expires.
func TestUnixRounding_FavoursTheBlocklist(t *testing.T) {
	base := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)

	if got := unixCeil(base); got == nil || *got != base.Unix() {
		t.Errorf("a whole second must not be inflated: %v", got)
	}
	if got := unixCeil(base.Add(1)); got == nil || *got != base.Unix()+1 {
		t.Errorf("a fractional deadline must round up, got %v", got)
	}
	if got := unixCeil(base.Add(999999999)); got == nil || *got != base.Unix()+1 {
		t.Errorf("just under the next second must round up, got %v", got)
	}
	if unixCeil(time.Time{}) != nil {
		t.Error("the zero time must produce NULL — a permanent entry, not one at the epoch")
	}
	if got := unixFloor(base.Add(999999999)); got != base.Unix() {
		t.Errorf("now must round down so the entry is honoured through its last second, got %d", got)
	}
}

// A cutoff of zero means "everything up to now", not "the epoch" — which would
// revoke nothing, since no token has an iat before 1970.
func TestOrNow_ZeroCutoffMeansNow(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	if got := orNow(time.Time{}, now); !got.Equal(now) {
		t.Errorf("orNow(zero) = %v, want %v", got, now)
	}
	earlier := now.Add(-time.Hour)
	if got := orNow(earlier, now); !got.Equal(earlier) {
		t.Errorf("orNow must not override an explicit cutoff, got %v", got)
	}
}

// Misreading pgx as SQLite makes every statement fail against Postgres — the
// dialect picks both the SQL and the placeholder style (audit JB-6).
func TestResolveIsPG_ClassifiesByPackagePath(t *testing.T) {
	for path, want := range map[string]bool{
		"github.com/jackc/pgx/v5/stdlib": true,
		"github.com/lib/pq":              true,
		"modernc.org/sqlite":             false,
		"github.com/mattn/go-sqlite3":    false,
	} {
		if got := isPostgresDriver(path, "Driver"); got != want {
			t.Errorf("isPostgresDriver(%q) = %v, want %v", path, got, want)
		}
	}

	// The explicit override wins, and an unrecognised one falls through to
	// detection rather than silently forcing a dialect.
	for name, want := range map[string]bool{
		"postgres": true, "postgresql": true, "pgx": true, "PGX": true,
		"sqlite": false, "sqlite3": false, "": false, "mysql": false,
	} {
		if got := resolveIsPG(name, nil); got != want {
			t.Errorf("resolveIsPG(%q) = %v, want %v", name, got, want)
		}
	}
}
