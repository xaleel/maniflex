package maniflextest_test

// The gap this closes: a consumer could not test expiry at all. Every path to
// it measured against the wall clock, so "the replay window closes after an
// hour" meant sleeping for an hour. Neither of these tests could be written
// before, and the framework's own suite shows it — every idempotency and cache
// test in the repository sets a long TTL and asserts the hit.

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/maniflextest"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/idempotency"
)

// ANCHOR: expiry
func TestIdempotencyWindowClosesOnceTheClockPasses(t *testing.T) {
	clock := maniflextest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := maniflex.NewMemoryCache(maniflex.WithCacheClock(clock.Now))

	server := maniflextest.New(t, maniflextest.Options{
		Models: []any{Widget{}},
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Deserialize.Register(
				idempotency.Middleware(idempotency.Config{Store: cache, TTL: time.Hour}),
				maniflex.AtPosition(maniflex.After),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	key := maniflextest.Header("Idempotency-Key", "order-1")
	body := map[string]any{"name": "widget"}

	server.POST("/widgets", body, key).AssertStatus(http.StatusCreated)
	server.POST("/widgets", body, key).AssertStatus(http.StatusCreated) // replayed
	assertWidgetCount(t, server, 1)

	clock.Advance(2 * time.Hour) // the replay window closes

	server.POST("/widgets", body, key).AssertStatus(http.StatusCreated)
	assertWidgetCount(t, server, 2)
}

// ANCHOR_END: expiry

func assertWidgetCount(t *testing.T, server *maniflextest.Server, want int) {
	t.Helper()
	got := maniflextest.DecodeDataList[Widget](
		server.GET("/widgets").AssertStatus(http.StatusOK),
	)
	if len(got) != want {
		t.Fatalf("widget count: got %d, want %d", len(got), want)
	}
}

// The same seam on the other side of the pipeline: a rate limit that lifts when
// its window passes, asserted without waiting out a real minute.
func TestRateLimitWindowLiftsOnceTheClockPasses(t *testing.T) {
	clock := maniflextest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	server := maniflextest.New(t, maniflextest.Options{
		Models: []any{Widget{}},
		Setup: func(app *maniflex.Server) {
			app.Pipeline.DB.Register(dbmw.RateLimit(dbmw.RateLimitConfig{
				RequestsPerMinute: 2,
				Clock:             clock.Now,
			}))
		},
	})

	for range 2 {
		server.GET("/widgets").AssertStatus(http.StatusOK)
	}
	server.GET("/widgets").AssertStatus(http.StatusTooManyRequests)

	clock.Advance(2 * time.Minute)

	server.GET("/widgets").AssertStatus(http.StatusOK)
}
