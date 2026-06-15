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
	client *goredis.Client
	prefix string
}

// NewRateLimitBackend returns a backend that stores counters under
// prefix:<key>. prefix may be empty.
func NewRateLimitBackend(client *goredis.Client, prefix string) *RateLimitBackend {
	return &RateLimitBackend{client: client, prefix: prefix}
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
	k := b.fullKey(key)
	pipe := b.client.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.ExpireNX(ctx, k, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("redis ratelimit incr: %w", err)
	}
	return incr.Val(), nil
}
