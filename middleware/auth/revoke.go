package auth

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/xaleel/maniflex"
)

// Revoker is a blocklist of JWTs that must no longer be accepted, even though
// they are correctly signed and not yet expired.
//
// A JWT is self-contained by design: once minted it is valid until its exp,
// and nothing the server does can take it back. That is fine until a user logs
// out, changes their password, or has their account compromised — at which
// point "valid until exp" is exactly wrong. A Revoker restores the server's
// say in the matter, at the cost of a lookup per request.
//
// It revokes at two granularities, and both are needed:
//
//   - One token, by its jti — ordinary logout, on this device only.
//   - Every token of one user issued before a cutoff — a password change, or
//     a compromised account, where the outstanding jti values are unknown and
//     unknowable. This is "log out everywhere".
//
// Wire one up with JWTOptions.Revoker. Implementations must be safe for
// concurrent use.
//
// # Failing closed
//
// Every method returns an error, and that is deliberate. When a lookup fails,
// the framework refuses the request rather than treating "I could not check"
// as "not revoked" — a blocklist that fails open silently un-revokes every
// logged-out token for the duration of a store outage, which is the moment it
// is most likely to matter. This is why Revoker is its own interface rather
// than a reuse of maniflex.CacheStore, whose Get reports a miss and an outage
// identically.
type Revoker interface {
	// RevokeToken blocks a single token by its jti. expiresAt is the token's own
	// exp: after it, the token is refused for being expired anyway, so the
	// implementation may drop the entry then and must not grow without bound.
	RevokeToken(ctx context.Context, jti string, expiresAt time.Time) error

	// IsTokenRevoked reports whether this jti has been revoked.
	IsTokenRevoked(ctx context.Context, jti string) (bool, error)

	// RevokeUser blocks every token for userID issued before cutoff — normally
	// time.Now(), so every token in existence right now. retainUntil is when the
	// record may be dropped: set it past the exp of the longest-lived token that
	// could still be outstanding (i.e. now + your token TTL), since dropping it
	// earlier silently un-revokes those tokens.
	RevokeUser(ctx context.Context, userID string, cutoff, retainUntil time.Time) error

	// UserCutoff returns the cutoff set by RevokeUser for userID, or the zero
	// time when the user has none. A token whose iat is before a non-zero
	// cutoff is refused.
	UserCutoff(ctx context.Context, userID string) (time.Time, error)
}

// Error codes the revocation check can abort with. They are distinct so a
// client (and a dashboard) can tell "you are logged out" from "we could not
// find out", which have opposite remedies.
const (
	// CodeTokenRevoked means the token was explicitly revoked. The client should
	// discard it and log in again.
	CodeTokenRevoked = "TOKEN_REVOKED"

	// CodeTokenNotRevocable means the token carries no jti, so it cannot be
	// revoked and therefore cannot be trusted while revocation is switched on.
	// It is a minting bug, not a client error.
	CodeTokenNotRevocable = "TOKEN_NOT_REVOCABLE"

	// CodeRevocationUnavailable means the blocklist could not be consulted. The
	// token may well be fine; the server declines to guess. Unlike the other
	// two this is a 503, because it is the server that is broken, not the
	// credential — a 401 here would send every healthy client into a re-login
	// storm during a store outage.
	CodeRevocationUnavailable = "REVOCATION_UNAVAILABLE"
)

// checkRevocation consults the blocklist for a token that has already passed
// signature and registered-claim validation. It returns the status and error to
// abort with, or (0, "", "") when the token is still good.
//
// Both granularities are checked: the token's own jti, then the user-level
// cutoff. The user lookup is skipped for a token with no subject, since there
// is no user to look up.
func checkRevocation(ctx context.Context, rev Revoker, claims map[string]any, jti, userID string) (int, string, string) {
	if jti == "" {
		// Revocation is on, and this token is outside it. Accepting it would
		// make the blocklist bypassable by simply not minting a jti.
		return http.StatusUnauthorized, CodeTokenNotRevocable,
			"token has no jti claim and cannot be revoked"
	}

	revoked, err := rev.IsTokenRevoked(ctx, jti)
	if err != nil {
		return http.StatusServiceUnavailable, CodeRevocationUnavailable,
			"could not verify token revocation status"
	}
	if revoked {
		return http.StatusUnauthorized, CodeTokenRevoked, "token has been revoked"
	}

	if userID == "" {
		return 0, "", ""
	}
	return checkUserCutoff(ctx, rev, claims, userID)
}

// checkUserCutoff applies a user-level "everything issued before T" revocation.
//
// A token with no iat cannot be placed relative to the cutoff, so it is refused
// whenever the user has one — but only then, so tokens without iat keep working
// for every user who has never revoked.
func checkUserCutoff(ctx context.Context, rev Revoker, claims map[string]any, userID string) (int, string, string) {
	cutoff, err := rev.UserCutoff(ctx, userID)
	if err != nil {
		return http.StatusServiceUnavailable, CodeRevocationUnavailable,
			"could not verify token revocation status"
	}
	if cutoff.IsZero() {
		return 0, "", ""
	}

	iatRaw, ok := claims["iat"]
	if !ok {
		return http.StatusUnauthorized, CodeTokenRevoked,
			"token has no iat claim and cannot be shown to postdate the revocation"
	}
	iatF, _ := toFloat64(iatRaw)
	if time.Unix(int64(iatF), 0).Before(cutoff) {
		return http.StatusUnauthorized, CodeTokenRevoked,
			"all tokens for this user issued before the revocation have been revoked"
	}
	return 0, "", ""
}

// ── MemoryRevoker ────────────────────────────────────────────────────────────

// MemoryRevoker is an in-process Revoker backed by a map, for single-replica
// deployments, development, and tests.
//
// Its limitation is structural, not incidental: the blocklist lives in one
// process, so a second replica does not see a logout performed by the first,
// and every entry is lost on restart — which un-revokes every still-unexpired
// token. Behind more than one replica, use a shared store; see
// middleware/auth/redis for a ready one.
//
// Entries are pruned lazily on lookup and in bulk every pruneInterval writes,
// so a long-running process does not accumulate expired records. There is no
// background goroutine.
type MemoryRevoker struct {
	mu      sync.RWMutex
	tokens  map[string]time.Time // jti → the moment the entry may be dropped
	users   map[string]userRevocation
	writes  int
	nowFunc func() time.Time // swappable for tests
}

// userRevocation is a user-level cutoff plus its own retention deadline.
type userRevocation struct {
	cutoff      time.Time
	retainUntil time.Time
}

// pruneInterval is how many writes pass between full sweeps. Lookups prune the
// entry they touch regardless, so this only bounds the memory held by entries
// nobody asks about.
const pruneInterval = 128

// NewMemoryRevoker returns an empty in-process Revoker.
func NewMemoryRevoker() *MemoryRevoker {
	return &MemoryRevoker{
		tokens:  make(map[string]time.Time),
		users:   make(map[string]userRevocation),
		nowFunc: time.Now,
	}
}

func (m *MemoryRevoker) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now()
}

// RevokeToken implements Revoker.
func (m *MemoryRevoker) RevokeToken(_ context.Context, jti string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[jti] = expiresAt
	m.writes++
	if m.writes%pruneInterval == 0 {
		m.pruneLocked()
	}
	return nil
}

// IsTokenRevoked implements Revoker.
func (m *MemoryRevoker) IsTokenRevoked(_ context.Context, jti string) (bool, error) {
	m.mu.RLock()
	expiresAt, ok := m.tokens[jti]
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	if !expiresAt.IsZero() && m.now().After(expiresAt) {
		// The token is expired on its own terms; the entry has done its job.
		m.mu.Lock()
		delete(m.tokens, jti)
		m.mu.Unlock()
		return false, nil
	}
	return true, nil
}

// RevokeUser implements Revoker.
func (m *MemoryRevoker) RevokeUser(_ context.Context, userID string, cutoff, retainUntil time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Never move a cutoff backwards: a later revocation supersedes an earlier
	// one, but an out-of-order call must not resurrect tokens.
	if existing, ok := m.users[userID]; ok && existing.cutoff.After(cutoff) {
		cutoff = existing.cutoff
	}
	m.users[userID] = userRevocation{cutoff: cutoff, retainUntil: retainUntil}
	m.writes++
	if m.writes%pruneInterval == 0 {
		m.pruneLocked()
	}
	return nil
}

// UserCutoff implements Revoker.
func (m *MemoryRevoker) UserCutoff(_ context.Context, userID string) (time.Time, error) {
	m.mu.RLock()
	rec, ok := m.users[userID]
	m.mu.RUnlock()
	if !ok {
		return time.Time{}, nil
	}
	if !rec.retainUntil.IsZero() && m.now().After(rec.retainUntil) {
		m.mu.Lock()
		delete(m.users, userID)
		m.mu.Unlock()
		return time.Time{}, nil
	}
	return rec.cutoff, nil
}

// pruneLocked drops every entry past its retention deadline. The caller holds
// the write lock.
func (m *MemoryRevoker) pruneLocked() {
	now := m.now()
	for jti, expiresAt := range m.tokens {
		if !expiresAt.IsZero() && now.After(expiresAt) {
			delete(m.tokens, jti)
		}
	}
	for userID, rec := range m.users {
		if !rec.retainUntil.IsZero() && now.After(rec.retainUntil) {
			delete(m.users, userID)
		}
	}
}

// Len reports how many token and user entries are currently held. It is for
// tests and diagnostics.
func (m *MemoryRevoker) Len() (tokens, users int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tokens), len(m.users)
}

// ── Logout endpoints ─────────────────────────────────────────────────────────

// Logout returns a ready-to-mount action that revokes the calling request's own
// token — ordinary "log out on this device".
//
// It reads the jti and exp of the token the caller authenticated with, so it
// must be mounted behind the same Auth middleware as everything else; an
// unauthenticated request never reaches the handler. Responds 204.
//
//	rev := auth.NewMemoryRevoker()
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{Revoker: rev}))
//	server.Action(auth.Logout(rev, "/logout"))
//
// Pass an empty path for the default, "/logout".
func Logout(rev Revoker, path string) maniflex.ActionConfig {
	if path == "" {
		path = "/logout"
	}
	return maniflex.ActionConfig{
		Method:  http.MethodPost,
		Path:    path,
		Tags:    []string{"Auth"},
		Summary: "Revoke the current token",
		Responses: map[int]*maniflex.OASSchema{
			204: nil,
			401: nil,
		},
		Handler: func(ctx *maniflex.ServerContext) error {
			if ctx.Auth == nil || ctx.Auth.SessionID == "" {
				ctx.Abort(http.StatusUnauthorized, CodeTokenNotRevocable,
					"no revocable token on this request")
				return nil
			}
			if err := rev.RevokeToken(ctx.Ctx, ctx.Auth.SessionID, tokenExpiry(ctx.Auth.Claims)); err != nil {
				// Report the failure. A logout that silently did nothing is
				// worse than one that failed, because the caller believes it.
				ctx.Abort(http.StatusServiceUnavailable, CodeRevocationUnavailable,
					"could not revoke the token")
				return nil
			}
			ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusNoContent}
			return nil
		},
	}
}

// LogoutAll returns a ready-to-mount action that revokes every token belonging
// to the calling user — "log out everywhere", the one to call after a password
// change.
//
// retain is how long the cutoff is kept, and must be at least as long as the
// lifetime of the longest-lived token you mint: dropping the record earlier
// un-revokes any token still outstanding. When zero it defaults to 24h.
//
//	server.Action(auth.LogoutAll(rev, "/logout-all", 24*time.Hour))
//
// Pass an empty path for the default, "/logout-all".
func LogoutAll(rev Revoker, path string, retain time.Duration) maniflex.ActionConfig {
	if path == "" {
		path = "/logout-all"
	}
	if retain <= 0 {
		retain = 24 * time.Hour
	}
	return maniflex.ActionConfig{
		Method:  http.MethodPost,
		Path:    path,
		Tags:    []string{"Auth"},
		Summary: "Revoke every token for the current user",
		Responses: map[int]*maniflex.OASSchema{
			204: nil,
			401: nil,
		},
		Handler: func(ctx *maniflex.ServerContext) error {
			if ctx.Auth == nil || ctx.Auth.UserID == "" {
				ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
					"no authenticated user on this request")
				return nil
			}
			now := time.Now()
			if err := rev.RevokeUser(ctx.Ctx, ctx.Auth.UserID, now, now.Add(retain)); err != nil {
				ctx.Abort(http.StatusServiceUnavailable, CodeRevocationUnavailable,
					"could not revoke the user's tokens")
				return nil
			}
			ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusNoContent}
			return nil
		},
	}
}

// tokenExpiry reads the exp claim as a time, returning the zero time when it is
// absent or unreadable. A zero expiry makes the blocklist entry permanent,
// which is the safe direction: the alternative is dropping it while the token
// is still usable.
func tokenExpiry(claims map[string]any) time.Time {
	exp, ok := claims["exp"]
	if !ok {
		return time.Time{}
	}
	expF, ok := toFloat64(exp)
	if !ok || expF <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(expF), 0)
}
