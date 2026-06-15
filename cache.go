package maniflex

import (
	"context"
	"sync"
	"time"
)

// CacheStore is a generic, TTL-aware key/value cache shared by middleware that
// needs cross-request memoisation (idempotency keys, rate-limit windows,
// per-user response caches, etc.). Implementations must be safe for concurrent
// use.
//
// Get returns ok=false when no entry exists for the key or it has expired.
// Set persists value under key for at most ttl.
// Delete removes the entry under key; it is a no-op when the key is absent.
type CacheStore interface {
	Get(ctx context.Context, key string) (any, bool)
	Set(ctx context.Context, key string, value any, ttl time.Duration)
	Delete(ctx context.Context, key string)
}

// MemoryCache is a process-local CacheStore implementation. Suitable for
// single-instance deployments and tests. For multi-replica deployments back
// the cache with Redis or another shared store by implementing CacheStore
// against it.
//
// MemoryCache evicts expired entries lazily on Get and, to bound memory for
// write-mostly keys (rate-limit windows, idempotency replays) that are never
// queried again, sweeps the map every memoryCachePruneEvery inserts. The
// pattern mirrors the rate-limiter (see middleware/db query.go §11B.5): no
// background goroutine is started, so a test that constructs many caches
// doesn't leak janitors that the testing harness would have to wait on.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]memCacheEntry
	inserts int
}

// memoryCachePruneEvery controls how often Set sweeps for expired entries.
// 128 keeps the amortised per-call cost effectively constant for a million
// inserts (≈ 8000 sweeps, each O(n)) while bounding memory growth.
const memoryCachePruneEvery = 128

type memCacheEntry struct {
	value     any
	expiresAt time.Time
}

// NewMemoryCache returns a ready-to-use in-process CacheStore.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[string]memCacheEntry)}
}

// Get implements CacheStore.
func (m *MemoryCache) Get(_ context.Context, key string) (any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(m.entries, key)
		return nil, false
	}
	return e.value, true
}

// Set implements CacheStore.
func (m *MemoryCache) Set(_ context.Context, key string, value any, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, existing := m.entries[key]; !existing {
		m.inserts++
		if m.inserts >= memoryCachePruneEvery {
			m.pruneLocked()
			m.inserts = 0
		}
	}
	m.entries[key] = memCacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
}

// Delete implements CacheStore.
func (m *MemoryCache) Delete(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// pruneLocked removes every expired entry. Must be called with m.mu held.
func (m *MemoryCache) pruneLocked() {
	now := time.Now()
	for k, e := range m.entries {
		if now.After(e.expiresAt) {
			delete(m.entries, k)
		}
	}
}