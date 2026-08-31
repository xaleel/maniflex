package maniflex

// A Clock is how a test moves time without sleeping. The zero value has to keep
// behaving as the framework did before one could be supplied, or adding the
// seam changes production behaviour for everyone who never sets it.
//
//	go test . -run TestClock

import (
	"testing"
	"time"
)

func TestClock_NilReadsTheWallClock(t *testing.T) {
	var c Clock

	before := time.Now()
	got := c.Now()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Fatalf("nil Clock returned %v, want a time within [%v, %v]", got, before, after)
	}
}

func TestClock_NonNilIsAsked(t *testing.T) {
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	calls := 0
	c := Clock(func() time.Time { calls++; return fixed })

	if got := c.Now(); !got.Equal(fixed) {
		t.Errorf("Now() = %v, want %v", got, fixed)
	}
	if calls != 1 {
		t.Errorf("the clock was called %d times, want 1", calls)
	}
}

// Now must read the clock on every call, not memoise the first answer — a fake
// that advances between reads is the whole point.
func TestClock_IsReadOnEveryCall(t *testing.T) {
	tick := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Clock(func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	})

	first, second := c.Now(), c.Now()
	if !second.After(first) {
		t.Fatalf("second read %v did not advance past the first %v", second, first)
	}
}
