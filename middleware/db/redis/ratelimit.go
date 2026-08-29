// Package redis provides a Redis-backed implementation of
// middleware/db.RateLimitBackend so that multiple replicas share one
// rate-limit window.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// RateLimitBackend is a Redis-backed counter for middleware/db.RateLimit.
//
// Each Increment performs INCR on the key and, when the value is 1 (i.e. the
// key was just created), sets the expiration to window. This produces a fixed
// window aligned to the first request in each window.
//
//	rb := redis.NewRateLimitBackend(client, "myapp:ratelimit")
//	server.Pipeline.DB.Register(
//	    db.RateLimit(db.RateLimitConfig{
//	        RequestsPerMinute: 60,
//	        Backend:           rb,
//	    }),
//	)
type RateLimitBackend struct {
	ops    counterOps
	prefix string
}

// counterOps is the one Redis operation the counter needs, behind a seam so the
// key composition and error handling above it can be driven by a fake without a
// live Redis — mirroring the jobs/redis and events/redis seams.
//
// It is one method rather than an INCR and an EXPIRE, because the two must
// reach Redis together: a failure between them would leave a counter with no
// TTL, which never resets and so bars the client for ever.
type counterOps interface {
	// IncrExpireNX increments the counter at key and, only when the key was
	// just created, pins its TTL to window.
	IncrExpireNX(ctx context.Context, key string, window time.Duration) (int64, error)
}

// redisCounterOps is the production implementation, backed by a real client.
type redisCounterOps struct{ client *goredis.Client }

func (r redisCounterOps) IncrExpireNX(ctx context.Context, key string, window time.Duration) (int64, error) {
	pipe := r.client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// NewRateLimitBackend returns a backend that stores counters under
// prefix:<key>. prefix may be empty.
func NewRateLimitBackend(client *goredis.Client, prefix string) *RateLimitBackend {
	return &RateLimitBackend{ops: redisCounterOps{client: client}, prefix: prefix}
}

func (b *RateLimitBackend) fullKey(key string) string {
	if b.prefix == "" {
		return key
	}
	return b.prefix + ":" + key
}

// Increment atomically increments the counter for key and, on first creation,
// pins its TTL to window. Subsequent increments within the same window do not
// extend the TTL, giving a fixed window aligned to the first request.
// Requires Redis 7.0+ (uses EXPIRE … NX).
func (b *RateLimitBackend) Increment(ctx context.Context, key string, window time.Duration) (int64, error) {
	n, err := b.ops.IncrExpireNX(ctx, b.fullKey(key), window)
	if err != nil {
		return 0, fmt.Errorf("redis ratelimit incr: %w", err)
	}
	return n, nil
}
