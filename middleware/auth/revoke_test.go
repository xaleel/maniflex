package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/xaleel/maniflex/middleware/auth"
)

// Compile-time proof that the shipped in-process implementation satisfies the
// interface the middleware consumes.
var _ auth.Revoker = (*auth.MemoryRevoker)(nil)

func TestMemoryRevoker_TokenBlocklist(t *testing.T) {
	ctx := context.Background()
	rev := auth.NewMemoryRevoker()

	if revoked, err := rev.IsTokenRevoked(ctx, "jti-1"); err != nil || revoked {
		t.Fatalf("fresh revoker: IsTokenRevoked = %v, %v; want false, nil", revoked, err)
	}

	if err := rev.RevokeToken(ctx, "jti-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if revoked, err := rev.IsTokenRevoked(ctx, "jti-1"); err != nil || !revoked {
		t.Errorf("after RevokeToken: IsTokenRevoked = %v, %v; want true, nil", revoked, err)
	}
	// Revoking one token must not touch another.
	if revoked, _ := rev.IsTokenRevoked(ctx, "jti-2"); revoked {
		t.Error("revoking jti-1 also revoked jti-2")
	}
}

// An entry whose token has expired is useless — the token is refused for being
// expired anyway — so it must not be retained forever.
func TestMemoryRevoker_ExpiredEntryIsDropped(t *testing.T) {
	ctx := context.Background()
	rev := auth.NewMemoryRevoker()

	if err := rev.RevokeToken(ctx, "jti-old", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	revoked, err := rev.IsTokenRevoked(ctx, "jti-old")
	if err != nil || revoked {
		t.Errorf("expired entry: IsTokenRevoked = %v, %v; want false, nil", revoked, err)
	}
	if tokens, _ := rev.Len(); tokens != 0 {
		t.Errorf("expired entry left %d records behind, want 0", tokens)
	}
}

func TestMemoryRevoker_UserCutoff(t *testing.T) {
	ctx := context.Background()
	rev := auth.NewMemoryRevoker()

	if cutoff, err := rev.UserCutoff(ctx, "user-1"); err != nil || !cutoff.IsZero() {
		t.Fatalf("fresh revoker: UserCutoff = %v, %v; want zero time, nil", cutoff, err)
	}

	now := time.Now().Truncate(time.Second)
	if err := rev.RevokeUser(ctx, "user-1", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	got, err := rev.UserCutoff(ctx, "user-1")
	if err != nil || !got.Equal(now) {
		t.Errorf("UserCutoff = %v, %v; want %v, nil", got, err, now)
	}
	if other, _ := rev.UserCutoff(ctx, "user-2"); !other.IsZero() {
		t.Errorf("revoking user-1 set a cutoff on user-2: %v", other)
	}
}

// An out-of-order call must not move a cutoff backwards — that would resurrect
// tokens a later revocation had already killed.
func TestMemoryRevoker_CutoffNeverMovesBackwards(t *testing.T) {
	ctx := context.Background()
	rev := auth.NewMemoryRevoker()

	later := time.Now().Truncate(time.Second)
	earlier := later.Add(-time.Hour)

	if err := rev.RevokeUser(ctx, "u", later, later.Add(time.Hour)); err != nil {
		t.Fatalf("RevokeUser(later): %v", err)
	}
	if err := rev.RevokeUser(ctx, "u", earlier, later.Add(time.Hour)); err != nil {
		t.Fatalf("RevokeUser(earlier): %v", err)
	}
	got, _ := rev.UserCutoff(ctx, "u")
	if !got.Equal(later) {
		t.Errorf("cutoff moved backwards to %v; want it held at %v", got, later)
	}
}

// A cutoff past its retention deadline is dropped, exactly like a token entry.
func TestMemoryRevoker_ExpiredCutoffIsDropped(t *testing.T) {
	ctx := context.Background()
	rev := auth.NewMemoryRevoker()

	past := time.Now().Add(-time.Hour)
	if err := rev.RevokeUser(ctx, "u", past, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if cutoff, err := rev.UserCutoff(ctx, "u"); err != nil || !cutoff.IsZero() {
		t.Errorf("retained-past cutoff = %v, %v; want zero time, nil", cutoff, err)
	}
	if _, users := rev.Len(); users != 0 {
		t.Errorf("expired cutoff left %d records behind, want 0", users)
	}
}
