package maniflex

// Expiry is the behaviour a consumer most wants to test and least can: every
// idempotency replay window and cached read in the framework lands in a
// MemoryCache, whose TTL was measured against the wall clock. Testing "the
// replay window closes after 24h" meant sleeping for 24 hours.
//
//	go test . -run TestMemoryCacheClock

import (
	"context"
	"testing"
	"time"
)

// testClock is a minimal controllable clock. maniflextest exports the version
// consumers use; the core package cannot import it.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestMemoryCacheClock_EntryLivesUntilTheClockPassesItsTTL(t *testing.T) {
	clock := newTestClock()
	cache := NewMemoryCache(WithCacheClock(clock.Now))
	ctx := context.Background()

	cache.Set(ctx, "k", "v", time.Hour)

	clock.advance(59 * time.Minute)
	if _, ok := cache.Get(ctx, "k"); !ok {
		t.Fatal("entry expired before its TTL elapsed on the injected clock")
	}

	clock.advance(2 * time.Minute)
	if _, ok := cache.Get(ctx, "k"); ok {
		t.Fatal("entry survived past its TTL — Get is still reading the wall clock")
	}
}

// Anti-vacuity: the injected clock must not simply expire everything.
func TestMemoryCacheClock_AStandingClockNeverExpiresAnything(t *testing.T) {
	clock := newTestClock()
	cache := NewMemoryCache(WithCacheClock(clock.Now))
	ctx := context.Background()

	cache.Set(ctx, "k", "v", time.Nanosecond)
	if _, ok := cache.Get(ctx, "k"); !ok {
		t.Fatal("a one-nanosecond entry expired against a clock that has not moved")
	}
}

// The prune sweep runs on inserts rather than reads, so it has its own path to
// the clock — and a sweep measuring against the wall clock would evict entries
// the injected clock still considers live.
func TestMemoryCacheClock_PruneSweepUsesTheInjectedClock(t *testing.T) {
	clock := newTestClock()
	cache := NewMemoryCache(WithCacheClock(clock.Now))
	ctx := context.Background()

	cache.Set(ctx, "keep", "v", time.Hour)
	for i := range memoryCachePruneEvery + 1 {
		cache.Set(ctx, string(rune('a'+i%26))+time.Duration(i).String(), "v", time.Hour)
	}

	if _, ok := cache.Get(ctx, "keep"); !ok {
		t.Fatal("the prune sweep evicted a live entry — it is measuring against the wall clock")
	}
}

// No clock supplied must keep the wall-clock behaviour every existing caller has.
func TestMemoryCacheClock_ZeroValueStillUsesTheWallClock(t *testing.T) {
	cache := NewMemoryCache()
	ctx := context.Background()

	cache.Set(ctx, "k", "v", -time.Hour) // already expired against any real clock
	if _, ok := cache.Get(ctx, "k"); ok {
		t.Fatal("an entry with a negative TTL was served — the default is not reading the wall clock")
	}
}
