package e2e

// AU-3 — the SQL-backed auth.Revoker, against a real database on whichever lane
// is running.
//
// These mirror the MemoryRevoker tests case for case, because the two are
// interchangeable by contract: an app moving from one replica to several changes
// the constructor and nothing else. Where they diverge the SQL one must be the
// stricter, and the cutoff-ordering case below is the one that shows it —
// MemoryRevoker holds a lock across its read and write, Redis reads then writes
// and documents the race, and this one lets the statement decide.
//
//	go test ./e2e/ -run TestSQLRevoker

import (
	"context"
	stdsql "database/sql"
	"testing"
	"time"

	"github.com/xaleel/maniflex/middleware/auth"
	authsql "github.com/xaleel/maniflex/middleware/auth/sql"
)

// newSQLRevoker migrates a dedicated table pair and returns a Revoker over it,
// so tests never share a blocklist.
func newSQLRevoker(t *testing.T, prefix string, opts ...authsql.Option) (*authsql.Revoker, *stdsql.DB) {
	t.Helper()
	db := rawJobsDB(t)
	opts = append([]authsql.Option{authsql.WithTablePrefix(prefix)}, opts...)
	if err := authsql.Migrate(context.Background(), db, jobsDriver(), opts...); err != nil {
		t.Fatalf("migrate %s: %v", prefix, err)
	}
	return authsql.NewRevoker(db, opts...), db
}

// It has to actually satisfy the interface the middleware takes.
var _ auth.Revoker = (*authsql.Revoker)(nil)

func TestSQLRevoker_TokenBlocklist(t *testing.T) {
	rev, _ := newSQLRevoker(t, "tb_")
	ctx := context.Background()

	revoked, err := rev.IsTokenRevoked(ctx, "jti-1")
	if err != nil {
		t.Fatalf("IsTokenRevoked: %v", err)
	}
	if revoked {
		t.Fatal("an unknown jti must not be revoked")
	}

	if err := rev.RevokeToken(ctx, "jti-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if revoked, err = rev.IsTokenRevoked(ctx, "jti-1"); err != nil || !revoked {
		t.Fatalf("revoked=%v err=%v, want true/nil", revoked, err)
	}
	// One token, not the whole store.
	if revoked, err = rev.IsTokenRevoked(ctx, "jti-2"); err != nil || revoked {
		t.Fatalf("jti-2 revoked=%v err=%v, want false/nil", revoked, err)
	}
}

// A blocklist entry stops mattering once the token expires on its own terms —
// otherwise the table grows forever for no benefit.
func TestSQLRevoker_ExpiredEntryStopsBeingHonoured(t *testing.T) {
	rev, _ := newSQLRevoker(t, "te_")
	ctx := context.Background()

	if err := rev.RevokeToken(ctx, "old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	revoked, err := rev.IsTokenRevoked(ctx, "old")
	if err != nil {
		t.Fatalf("IsTokenRevoked: %v", err)
	}
	if revoked {
		t.Error("an entry past the token's own exp must not be honoured")
	}
}

// A token minted with no exp claim yields the zero time, which must mean
// "never drop" rather than "expired in 1970" — the latter would make the entry
// useless the moment it was written.
func TestSQLRevoker_ZeroExpiryIsPermanent(t *testing.T) {
	rev, _ := newSQLRevoker(t, "tz_")
	ctx := context.Background()

	if err := rev.RevokeToken(ctx, "forever", time.Time{}); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "forever"); err != nil || !revoked {
		t.Fatalf("revoked=%v err=%v, want true/nil", revoked, err)
	}
	// And the sweep must not collect it either.
	if _, _, err := rev.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "forever"); err != nil || !revoked {
		t.Fatalf("after prune: revoked=%v err=%v, want true/nil", revoked, err)
	}
}

// Re-revoking the same token can only extend the entry. A second call carrying a
// shorter deadline — a retry against a stale copy of the token, say — must not
// shorten how long the revocation is honoured.
func TestSQLRevoker_RepeatRevocationOnlyExtends(t *testing.T) {
	rev, _ := newSQLRevoker(t, "tr_")
	ctx := context.Background()
	far := time.Now().Add(time.Hour)

	if err := rev.RevokeToken(ctx, "j", far); err != nil {
		t.Fatalf("RevokeToken far: %v", err)
	}
	if err := rev.RevokeToken(ctx, "j", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeToken near: %v", err)
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "j"); err != nil || !revoked {
		t.Fatalf("a shorter repeat revocation shortened the entry: revoked=%v err=%v", revoked, err)
	}

	// And a permanent entry stays permanent when re-revoked with a deadline.
	if err := rev.RevokeToken(ctx, "p", time.Time{}); err != nil {
		t.Fatalf("RevokeToken permanent: %v", err)
	}
	if err := rev.RevokeToken(ctx, "p", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeToken permanent+near: %v", err)
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "p"); err != nil || !revoked {
		t.Fatalf("a permanent entry was given an expiry: revoked=%v err=%v", revoked, err)
	}
}

func TestSQLRevoker_UserCutoff(t *testing.T) {
	rev, _ := newSQLRevoker(t, "uc_")
	ctx := context.Background()

	cutoff, err := rev.UserCutoff(ctx, "u1")
	if err != nil {
		t.Fatalf("UserCutoff: %v", err)
	}
	if !cutoff.IsZero() {
		t.Fatalf("a user with no revocation must report the zero time, got %v", cutoff)
	}

	now := time.Now()
	if err := rev.RevokeUser(ctx, "u1", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	cutoff, err = rev.UserCutoff(ctx, "u1")
	if err != nil {
		t.Fatalf("UserCutoff after revoke: %v", err)
	}
	if cutoff.IsZero() {
		t.Fatal("the cutoff was not stored")
	}
	// Second resolution is the contract; the claim it is compared against has no
	// more. It must not round DOWN, or a token minted in the same second as the
	// revocation survives "log out everywhere".
	if cutoff.Before(now.Truncate(time.Second)) {
		t.Errorf("cutoff %v predates the revocation instant %v", cutoff, now)
	}
	if delta := cutoff.Sub(now); delta > time.Second || delta < -time.Second {
		t.Errorf("cutoff %v is not within a second of %v", cutoff, now)
	}
}

// The rule the interface states: a later revocation supersedes an earlier one,
// but an out-of-order call must never resurrect tokens. Here it is the statement
// that enforces it, not a lock the caller holds.
func TestSQLRevoker_CutoffNeverMovesBackwards(t *testing.T) {
	rev, _ := newSQLRevoker(t, "ub_")
	ctx := context.Background()

	late := time.Now()
	early := late.Add(-time.Hour)

	if err := rev.RevokeUser(ctx, "u1", late, late.Add(24*time.Hour)); err != nil {
		t.Fatalf("RevokeUser late: %v", err)
	}
	if err := rev.RevokeUser(ctx, "u1", early, early.Add(24*time.Hour)); err != nil {
		t.Fatalf("RevokeUser early: %v", err)
	}
	cutoff, err := rev.UserCutoff(ctx, "u1")
	if err != nil {
		t.Fatalf("UserCutoff: %v", err)
	}
	if cutoff.Before(late.Truncate(time.Second)) {
		t.Errorf("cutoff moved backwards to %v; the later revocation was at %v", cutoff, late)
	}
}

// Retention is what keeps a cutoff meaningful. Dropping it early silently
// un-revokes every token that was still outstanding.
func TestSQLRevoker_CutoffStopsAtRetention(t *testing.T) {
	rev, _ := newSQLRevoker(t, "ur_")
	ctx := context.Background()

	now := time.Now()
	if err := rev.RevokeUser(ctx, "u1", now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	cutoff, err := rev.UserCutoff(ctx, "u1")
	if err != nil {
		t.Fatalf("UserCutoff: %v", err)
	}
	if !cutoff.IsZero() {
		t.Errorf("a cutoff past its retention must read as absent, got %v", cutoff)
	}
}

// Prune removes what is dead and nothing else. The row still being honoured is
// the one that matters: deleting it would un-revoke a live token.
func TestSQLRevoker_PruneRemovesOnlyExpiredRows(t *testing.T) {
	rev, db := newSQLRevoker(t, "pr_")
	ctx := context.Background()
	now := time.Now()

	if err := rev.RevokeToken(ctx, "dead", now.Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeToken dead: %v", err)
	}
	if err := rev.RevokeToken(ctx, "live", now.Add(time.Hour)); err != nil {
		t.Fatalf("RevokeToken live: %v", err)
	}
	if err := rev.RevokeToken(ctx, "forever", time.Time{}); err != nil {
		t.Fatalf("RevokeToken forever: %v", err)
	}
	if err := rev.RevokeUser(ctx, "u-dead", now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("RevokeUser dead: %v", err)
	}
	if err := rev.RevokeUser(ctx, "u-live", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("RevokeUser live: %v", err)
	}

	tokens, users, err := rev.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if tokens != 1 {
		t.Errorf("pruned %d token rows, want 1", tokens)
	}
	if users != 1 {
		t.Errorf("pruned %d user rows, want 1", users)
	}

	// What survives is what the read path would still honour.
	assertRowCount(t, db, "pr_revoked_token", 2)
	assertRowCount(t, db, "pr_revoked_user", 1)
	if revoked, err := rev.IsTokenRevoked(ctx, "live"); err != nil || !revoked {
		t.Errorf("the live entry was pruned: revoked=%v err=%v", revoked, err)
	}
}

// The sweep is opportunistic, so it must not be load-bearing — but it does have
// to actually fire, or the table grows without bound on a busy server.
func TestSQLRevoker_OpportunisticSweepFires(t *testing.T) {
	rev, db := newSQLRevoker(t, "op_", authsql.WithPruneEvery(3))
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)

	// Two writes: below the interval, so nothing is swept yet.
	for _, jti := range []string{"a", "b"} {
		if err := rev.RevokeToken(ctx, jti, past); err != nil {
			t.Fatalf("RevokeToken %s: %v", jti, err)
		}
	}
	assertRowCount(t, db, "op_revoked_token", 2)

	// The third write crosses the interval and takes the dead rows with it.
	if err := rev.RevokeToken(ctx, "c", past); err != nil {
		t.Fatalf("RevokeToken c: %v", err)
	}
	assertRowCount(t, db, "op_revoked_token", 0)
}

func TestSQLRevoker_PruneEveryZeroDisablesTheSweep(t *testing.T) {
	rev, db := newSQLRevoker(t, "oz_", authsql.WithPruneEvery(0))
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)

	for _, jti := range []string{"a", "b", "c", "d", "e"} {
		if err := rev.RevokeToken(ctx, jti, past); err != nil {
			t.Fatalf("RevokeToken %s: %v", jti, err)
		}
	}
	assertRowCount(t, db, "oz_revoked_token", 5)
}

// Migrate runs on every boot and from every replica, so it must be idempotent.
func TestSQLRevoker_MigrateIsIdempotent(t *testing.T) {
	rev, db := newSQLRevoker(t, "mi_")
	ctx := context.Background()

	if err := rev.RevokeToken(ctx, "keep", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	for i := range 3 {
		if err := authsql.Migrate(ctx, db, jobsDriver(), authsql.WithTablePrefix("mi_")); err != nil {
			t.Fatalf("migrate pass %d: %v", i, err)
		}
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "keep"); err != nil || !revoked {
		t.Fatalf("re-migrating lost the blocklist: revoked=%v err=%v", revoked, err)
	}
}

// The failure mode the whole interface is shaped around: a store that cannot be
// reached must produce an error, never a clean "not revoked". The middleware
// turns that error into a 503; a false would silently un-revoke every logged-out
// token for the duration of the outage.
func TestSQLRevoker_UnreachableStoreReportsAnError(t *testing.T) {
	rev, db := newSQLRevoker(t, "er_")
	ctx := context.Background()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := rev.IsTokenRevoked(ctx, "j"); err == nil {
		t.Error("IsTokenRevoked returned nil error against a closed database")
	}
	if _, err := rev.UserCutoff(ctx, "u"); err == nil {
		t.Error("UserCutoff returned nil error against a closed database")
	}
	if err := rev.RevokeToken(ctx, "j", time.Now().Add(time.Hour)); err == nil {
		t.Error("RevokeToken returned nil error against a closed database")
	}
}

func assertRowCount(t *testing.T, db *stdsql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s holds %d rows, want %d", table, got, want)
	}
}
