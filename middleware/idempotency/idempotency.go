// Package idempotency provides a Deserialize-step middleware that makes POST
// requests safe to retry over flaky networks.
//
// Clients send an `Idempotency-Key` header alongside POST requests. The first
// request runs the pipeline normally; the response is cached keyed on the
// {user_id, model, operation, idempotency_key, body_hash} tuple. Subsequent
// requests bearing the same key and the same body short-circuit the pipeline
// and replay the cached response. Same key with a different body returns 422 —
// the contract is "an Idempotency-Key uniquely identifies one logical
// operation".
//
// Two concurrent first-misses with the same key are serialised by a Locker.
// The default in-process Locker (singleflight-style) guarantees that only one
// goroutine per process runs the pipeline; the others wait for it to complete
// and replay the cached response. For multi-replica deployments supply
// Config.Locker with a Redis SETNX-style implementation.
//
//	server.Pipeline.Deserialize.Register(
//	    idempotency.Middleware(idempotency.Config{
//	        Store: maniflex.NewMemoryCache(),
//	        TTL:   24 * time.Hour,
//	    }),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	    maniflex.AtPosition(maniflex.After),
//	)
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"maniflex"
)

// Locker serialises concurrent first-misses on the same idempotency cache key.
// Implementations must return acquired=true to exactly one caller per key per
// TTL window; concurrent callers either block until the holder releases
// (singleflight-style, the default) or are signalled that another holder is
// in flight (SETNX-style Redis adapters).
//
// When acquired=true the caller MUST invoke the returned release func once
// the cache entry has been written (or the work has failed). When
// acquired=false the caller should re-check the cache before running the
// pipeline itself.
type Locker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (acquired bool, release func(), err error)
}

// inProcessLocker is the default Locker. It blocks concurrent callers on a
// shared channel keyed by `key`; the first caller closes the channel when it
// releases, waking all waiters who then re-check the cache. Sufficient for
// single-replica deployments; supply Config.Locker for multi-replica.
type inProcessLocker struct {
	inflight sync.Map // map[string]chan struct{}
}

func newInProcessLocker() *inProcessLocker {
	return &inProcessLocker{}
}

func (l *inProcessLocker) Acquire(ctx context.Context, key string, _ time.Duration) (bool, func(), error) {
	ch := make(chan struct{})
	actual, loaded := l.inflight.LoadOrStore(key, ch)
	if !loaded {
		return true, func() {
			l.inflight.Delete(key)
			close(ch)
		}, nil
	}
	// Another caller already holds the slot. Wait for it to release (or for
	// the request context to be cancelled) and return acquired=false so the
	// caller knows to re-check the cache.
	select {
	case <-actual.(chan struct{}):
		return false, nil, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

// HeaderName is the HTTP request header read by the middleware.
const HeaderName = "Idempotency-Key"

// Entry is the cached snapshot of a previously-served response.
type Entry struct {
	maniflex.APIResponse
	BodyHash string
	StoredAt time.Time
}

// Config configures the idempotency middleware.
type Config struct {
	// Store is the cache backend. Required.
	Store maniflex.CacheStore
	// TTL is how long a cached response is replayable. Default: 24h.
	TTL time.Duration
	// KeyFunc derives the per-caller scope for the cache key.
	// Default: ctx.Auth.UserID, falling back to the remote IP.
	KeyFunc func(ctx *maniflex.ServerContext) string
	// HeaderRequired makes the Idempotency-Key header mandatory on every
	// request the middleware sees. When false (the default), requests without
	// the header pass through untouched.
	HeaderRequired bool
	// Locker serialises concurrent first-misses for the same cache key. When
	// nil, an in-process singleflight-style Locker is used — sufficient for
	// single-replica deployments. For multi-replica setups supply a Redis
	// SETNX-based implementation so two replicas don't both run the pipeline.
	Locker Locker
}

// Middleware returns the idempotency MiddlewareFunc.
//
// It is intended to be registered at AtPosition(After) on the Deserialize
// step, scoped to OpCreate, so that ctx.RawBody is populated by the default
// deserializer before this middleware computes the body hash.
func Middleware(cfg Config) maniflex.MiddlewareFunc {
	if cfg.Store == nil {
		panic("idempotency: Config.Store is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	keyFn := cfg.KeyFunc
	if keyFn == nil {
		keyFn = func(ctx *maniflex.ServerContext) string {
			if ctx.Auth != nil && ctx.Auth.UserID != "" {
				return ctx.Auth.UserID
			}
			return ctx.Request.RemoteAddr
		}
	}
	locker := cfg.Locker
	if locker == nil {
		locker = newInProcessLocker()
	}

	// replayCached pulls the entry from the store and sets ctx.Response.
	// Returns (true, nil) when a valid cached response was replayed, or when
	// a body-hash mismatch was reported via 422. Returns (false, nil) when
	// nothing is cached for the key.
	replayCached := func(ctx *maniflex.ServerContext, cacheKey, bodyHash string) (bool, error) {
		raw, ok := cfg.Store.Get(ctx.Ctx, cacheKey)
		if !ok {
			return false, nil
		}
		entry, ok := raw.(Entry)
		if !ok {
			ctx.Abort(http.StatusInternalServerError, "IDEMPOTENCY_CACHE_CORRUPT",
				"cached idempotency entry has unexpected type")
			return true, nil
		}
		if entry.BodyHash != bodyHash {
			ctx.Abort(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED",
				"Idempotency-Key has already been used with a different request body")
			return true, nil
		}
		ctx.Writer.Header().Set("Idempotent-Replayed", "true")
		ctx.Response = &maniflex.APIResponse{
			StatusCode: entry.StatusCode,
			Data:       entry.Data,
			Error:      entry.Error,
			Meta:       entry.Meta,
		}
		return true, nil
	}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		idemKey := ctx.Request.Header.Get(HeaderName)
		if idemKey == "" {
			if cfg.HeaderRequired {
				ctx.Abort(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED",
					"Idempotency-Key header is required for this operation")
				return nil
			}
			return next()
		}

		bodyHash := hashBody(ctx.RawBody)
		scope := "_:_"
		if ctx.Model != nil {
			scope = ctx.Model.Name + ":" + string(ctx.Operation)
		}
		cacheKey := keyFn(ctx) + ":" + scope + ":" + idemKey

		if handled, err := replayCached(ctx, cacheKey, bodyHash); handled || err != nil {
			return err
		}

		// Acquire a per-key lock so two simultaneous first-misses serialise:
		// the winner runs the pipeline and caches the response; losers wait
		// for the winner to finish, then replay from cache. A Locker error
		// (Redis network blip, ctx cancelled) is fail-open — run the
		// pipeline directly, mirroring pre-fix behaviour rather than 503ing.
		acquired, release, err := locker.Acquire(ctx.Ctx, cacheKey, cfg.TTL)
		if err != nil {
			return next()
		}
		if !acquired {
			// Someone else just finished — re-check the cache. If the holder
			// failed (no entry written), fall through to running ourselves.
			if handled, err := replayCached(ctx, cacheKey, bodyHash); handled || err != nil {
				return err
			}
			return next()
		}
		defer release()

		// Race check inside the lock: another caller may have completed
		// between our first Get and Acquire (in-process Locker only).
		if handled, err := replayCached(ctx, cacheKey, bodyHash); handled || err != nil {
			return err
		}

		if err := next(); err != nil {
			return err
		}

		// Only cache successful responses; retrying a failed write is the
		// whole point — if the first attempt 5xx'd we want the retry to
		// actually re-run the pipeline.
		if ctx.Response == nil ||
			ctx.Response.StatusCode < 200 ||
			ctx.Response.StatusCode >= 300 {
			return nil
		}

		cfg.Store.Set(ctx.Ctx, cacheKey, Entry{
			BodyHash: bodyHash,
			APIResponse: maniflex.APIResponse{
				StatusCode: ctx.Response.StatusCode,
				Data:       ctx.Response.Data,
				Error:      ctx.Response.Error,
				Meta:       ctx.Response.Meta,
			},
			StoredAt: time.Now().UTC(),
		}, cfg.TTL)
		return nil
	}
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}