package maniflex

import "time"

// Clock reports the current time. It exists so behaviour that turns on the
// passage of time — cache and idempotency expiry, rate-limit windows — can be
// exercised by a test that moves time instead of sleeping for it.
//
// A nil Clock reads the wall clock, so the zero value behaves exactly as the
// framework did before one could be supplied. Read it through Now rather than
// calling it directly, which is what makes nil safe at every use site.
//
// In tests, maniflextest.NewClock supplies a controllable one:
//
//	clock := maniflextest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
//	cache := maniflex.NewMemoryCache(maniflex.WithCacheClock(clock.Now))
//	// … make a request that populates the cache …
//	clock.Advance(25 * time.Hour)
//	// … the entry is now expired …
type Clock func() time.Time

// Now returns the clock's reading, or the wall clock when the Clock is nil.
// It asks on every call: a clock that advances between reads is the point.
func (c Clock) Now() time.Time {
	if c == nil {
		return time.Now()
	}
	return c()
}
