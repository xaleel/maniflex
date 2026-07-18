// Package redis provides a Redis-backed implementation of
// middleware/auth.Revoker, so that a logout performed on one replica is seen by
// every other replica — and survives a restart, which an in-process blocklist
// does not.
package redis

import (
	"context"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Revoker is a Redis-backed JWT blocklist for middleware/auth.
//
// Two key spaces are used, both with a TTL so entries expire on their own and
// the blocklist cannot grow without bound:
//
//	<prefix>:jti:<jti>   → "1", expiring at the token's own exp
//	<prefix>:user:<sub>  → the cutoff as a Unix timestamp, expiring at retainUntil
//
// Wire it into the JWT middleware:
//
//	rev := redis.NewRevoker(client, "myapp:revoked")
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{Revoker: rev}))
//	server.Action(auth.Logout(rev, ""))
//	server.Action(auth.LogoutAll(rev, "", 24*time.Hour))
//
// Errors are returned rather than swallowed, which is what lets the middleware
// fail closed: during a Redis outage requests are refused with 503 instead of
// every revoked token quietly becoming valid again.
type Revoker struct {
	client *goredis.Client
	prefix string
}

// NewRevoker returns a Revoker storing entries under prefix. prefix may be
// empty, though a namespace is recommended on a shared Redis.
func NewRevoker(client *goredis.Client, prefix string) *Revoker {
	return &Revoker{client: client, prefix: prefix}
}

func (r *Revoker) key(kind, id string) string {
	if r.prefix == "" {
		return kind + ":" + id
	}
	return r.prefix + ":" + kind + ":" + id
}

// ttlUntil converts a deadline into a TTL for SET. A zero deadline means "no
// expiry" (0 to Redis); a deadline already in the past yields a minimal TTL
// rather than 0, so a stale call cannot accidentally create a permanent entry.
func ttlUntil(deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return 0
	}
	if ttl := time.Until(deadline); ttl > 0 {
		return ttl
	}
	return time.Second
}

// RevokeToken implements auth.Revoker.
func (r *Revoker) RevokeToken(ctx context.Context, jti string, expiresAt time.Time) error {
	return r.client.Set(ctx, r.key("jti", jti), "1", ttlUntil(expiresAt)).Err()
}

// IsTokenRevoked implements auth.Revoker. A missing key is a clean "not
// revoked"; any other error is returned so the caller can fail closed.
func (r *Revoker) IsTokenRevoked(ctx context.Context, jti string) (bool, error) {
	err := r.client.Get(ctx, r.key("jti", jti)).Err()
	if errors.Is(err, goredis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RevokeUser implements auth.Revoker.
//
// The cutoff is only ever moved forward. Redis has no conditional "set if
// greater", so the current value is read first; the race this leaves is benign,
// since two concurrent revocations both mean "revoke everything up to about
// now" and either value satisfies both.
func (r *Revoker) RevokeUser(ctx context.Context, userID string, cutoff, retainUntil time.Time) error {
	key := r.key("user", userID)
	if existing, err := r.readCutoff(ctx, key); err != nil {
		return err
	} else if existing.After(cutoff) {
		cutoff = existing
	}
	return r.client.Set(ctx, key,
		strconv.FormatInt(cutoff.Unix(), 10), ttlUntil(retainUntil)).Err()
}

// UserCutoff implements auth.Revoker.
func (r *Revoker) UserCutoff(ctx context.Context, userID string) (time.Time, error) {
	return r.readCutoff(ctx, r.key("user", userID))
}

// readCutoff reads a stored cutoff, returning the zero time when the key is
// absent. A value that is not a Unix timestamp is treated as absent rather than
// as an error: it can only come from something else writing into this key
// space, and refusing every request for that user is a worse answer than
// ignoring it.
func (r *Revoker) readCutoff(ctx context.Context, key string) (time.Time, error) {
	raw, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	secs, convErr := strconv.ParseInt(raw, 10, 64)
	if convErr != nil {
		return time.Time{}, nil
	}
	return time.Unix(secs, 0), nil
}
