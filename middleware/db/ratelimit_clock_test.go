package db

// "Requests are allowed again once the window passes" is the assertion any app
// with a rate limit wants, and it was unreachable: the in-process window was
// measured against the wall clock, so testing it meant sleeping out a real
// minute.
//
//	go test ./middleware/db/ -run TestRateLimitClock

import (
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

func rateLimitClockFixture(clock maniflex.Clock) (RateLimitConfig, *rateLimiter) {
	return RateLimitConfig{RequestsPerMinute: 2, Clock: clock},
		&rateLimiter{windows: make(map[string]*window)}
}

func TestRateLimitClock_WindowResetsWhenTheClockPassesIt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg, limiter := rateLimitClockFixture(func() time.Time { return now })
	ctx := &maniflex.ServerContext{}
	const windowDur = time.Minute

	for i := 1; i <= 2; i++ {
		over, err := rateLimitCheck(ctx, cfg, limiter, "k", windowDur)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if over {
			t.Fatalf("request %d of an allowance of 2 was refused", i)
		}
	}

	over, err := rateLimitCheck(ctx, cfg, limiter, "k", windowDur)
	if err != nil {
		t.Fatal(err)
	}
	if !over {
		t.Fatal("the third request in a window of 2 was allowed")
	}

	// Anti-vacuity: while the clock stands still the limit must keep holding,
	// or "it resets" below is indistinguishable from "it never counted".
	if over, _ := rateLimitCheck(ctx, cfg, limiter, "k", windowDur); !over {
		t.Fatal("the limit lifted without the clock moving")
	}

	now = now.Add(windowDur + time.Second)
	if over, _ := rateLimitCheck(ctx, cfg, limiter, "k", windowDur); over {
		t.Fatal("the window did not reset once the clock passed it")
	}
}

func TestRateLimitClock_ZeroValueStillUsesTheWallClock(t *testing.T) {
	cfg, limiter := rateLimitClockFixture(nil)
	ctx := &maniflex.ServerContext{}

	// A window that closed an hour ago must be replaced on the first call
	// rather than counting against a stale one.
	limiter.windows["k"] = &window{resetAt: time.Now().Add(-time.Hour), count: 99}

	over, err := rateLimitCheck(ctx, cfg, limiter, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if over {
		t.Fatal("an expired window still refused the request — the default is not reading the wall clock")
	}
}
