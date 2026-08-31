package maniflextest

import (
	"sync"
	"time"
)

// Clock is a controllable clock for testing behaviour that turns on the passage
// of time — cache and idempotency expiry, rate-limit windows — without waiting
// for it.
//
// Hand its Now method to whatever measures the time, then move it:
//
//	clock := maniflextest.NewClock(time.Now())
//	cache := maniflex.NewMemoryCache(maniflex.WithCacheClock(clock.Now))
//
//	server := maniflextest.New(t, maniflextest.Options{
//	    Models: []any{Order{}},
//	    Setup: func(app *maniflex.Server) {
//	        app.Pipeline.Deserialize.Register(
//	            idempotency.Middleware(idempotency.Config{Store: cache, TTL: time.Hour}),
//	            maniflex.AtPosition(maniflex.After),
//	            maniflex.ForOperation(maniflex.OpCreate),
//	        )
//	    },
//	})
//
//	first := server.POST("/orders", body, maniflextest.Header("Idempotency-Key", "k"))
//	clock.Advance(2 * time.Hour) // the replay window closes
//
// It is safe for concurrent use: the server reads the clock on handler
// goroutines while the test advances it.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock stopped at start. It does not tick on its own — time
// moves only when the test says so, which is what makes the assertions
// deterministic.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now reports the clock's current instant. The method value satisfies
// maniflex.Clock and stays bound to this clock, so it can be handed to any
// component that takes one.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d. A negative d moves it back, which is
// occasionally what a test of clock skew wants.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute instant.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
