package jobs

import (
	"math"
	"testing"
	"time"
)

// The defect: a delay that came back negative, which callers add to time.Now()
// and so schedules the retry in the past — an immediate re-run instead of a
// backoff. Swept across the plausible configuration space rather than the one
// case the audit names, because Max is no defence: the old cap read `d > e.Max`
// and a negative d is not greater than anything.
func TestExponentialBackoff_NeverNegative(t *testing.T) {
	policies := []ExponentialBackoff{
		{},                  // zero value
		{Base: time.Second}, // uncapped
		{Base: time.Second, Max: 5 * time.Minute}, // the default policy
		{Base: 24 * time.Hour, Max: 5 * time.Minute},
		{Base: time.Nanosecond, Max: time.Hour},
		{Base: time.Hour, Max: time.Second}, // Base already past Max
		{Base: -time.Second, Max: -time.Minute},
	}
	for _, p := range policies {
		for attempt := range 200 {
			if d := p.Next(attempt); d < 0 {
				t.Fatalf("%+v attempt %d returned %v — the retry would be scheduled in the past", p, attempt, d)
			}
		}
	}
}

// The two configurations that actually went negative, pinned by name so a
// regression says which one broke.
func TestExponentialBackoff_OverflowCasesSaturate(t *testing.T) {
	tests := []struct {
		name    string
		policy  ExponentialBackoff
		attempt int
		want    time.Duration
	}{
		// Went to MinInt64 despite a 5-minute cap, at an attempt count a
		// MaxRetry of 20 reaches.
		{"daily base, 5m cap", ExponentialBackoff{Base: 24 * time.Hour, Max: 5 * time.Minute}, 17, 5 * time.Minute},
		// The default policy itself.
		{"default policy", ExponentialBackoff{Base: time.Second, Max: 5 * time.Minute}, 64, 5 * time.Minute},
		// Uncapped: saturates at the largest Duration instead of wrapping.
		{"uncapped", ExponentialBackoff{Base: time.Second}, 99, math.MaxInt64},
		{"uncapped, huge attempt", ExponentialBackoff{Base: time.Second}, math.MaxInt32, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Next(tc.attempt); got != tc.want {
				t.Fatalf("Next(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

// Anti-vacuity for the sweep above: returning a constant would satisfy
// "never negative", so pin the actual schedule.
func TestExponentialBackoff_DoublesThenHoldsAtMax(t *testing.T) {
	p := ExponentialBackoff{Base: time.Second, Max: 5 * time.Minute}
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for attempt, w := range want {
		if got := p.Next(attempt); got != w {
			t.Fatalf("Next(%d) = %v, want %v", attempt, got, w)
		}
	}
}

// A delay must never shrink as attempts accumulate — the property the whole
// policy exists to provide, and the one the overflow destroyed.
func TestExponentialBackoff_MonotonicNonDecreasing(t *testing.T) {
	for _, p := range []ExponentialBackoff{
		{Base: time.Second, Max: 5 * time.Minute},
		{Base: 24 * time.Hour},
		{Base: time.Nanosecond},
	} {
		prev := time.Duration(-1)
		for attempt := range 200 {
			d := p.Next(attempt)
			if d < prev {
				t.Fatalf("%+v: Next(%d) = %v is shorter than Next(%d) = %v", p, attempt, d, attempt-1, prev)
			}
			prev = d
		}
	}
}

// A struct literal that forgets Base used to mean "retry with no delay at all",
// the same hot loop the overflow produced. Zero now means the documented
// default, matching MaxRetryFor's 0→3.
func TestExponentialBackoff_ZeroBaseUsesTheDefault(t *testing.T) {
	zero := ExponentialBackoff{Max: time.Minute}
	explicit := ExponentialBackoff{Base: time.Second, Max: time.Minute}
	for attempt := range 10 {
		got, want := zero.Next(attempt), explicit.Next(attempt)
		if got != want {
			t.Fatalf("attempt %d: zero Base gave %v, explicit 1s gave %v", attempt, got, want)
		}
		if got == 0 {
			t.Fatalf("attempt %d: zero Base still yields no delay", attempt)
		}
	}
}

func TestExponentialBackoff_BaseBeyondMaxIsCapped(t *testing.T) {
	p := ExponentialBackoff{Base: time.Hour, Max: time.Second}
	if got := p.Next(0); got != time.Second {
		t.Fatalf("Next(0) = %v, want the 1s cap", got)
	}
}

func TestExponentialBackoff_NegativeAttemptTreatedAsFirst(t *testing.T) {
	p := ExponentialBackoff{Base: time.Second, Max: time.Minute}
	if got := p.Next(-5); got != time.Second {
		t.Fatalf("Next(-5) = %v, want %v", got, time.Second)
	}
}

// The default path a Job with no Backoff takes, driven through the real
// accessor rather than a hand-built policy.
func TestBackoffFor_DefaultPolicyStaysSane(t *testing.T) {
	p := BackoffFor(Job{})
	for attempt := range 200 {
		d := p.Next(attempt)
		if d <= 0 {
			t.Fatalf("default policy attempt %d returned %v", attempt, d)
		}
		if d > 5*time.Minute {
			t.Fatalf("default policy attempt %d returned %v, past its own 5m cap", attempt, d)
		}
		if !time.Now().Add(d).After(time.Now()) {
			t.Fatalf("default policy attempt %d schedules the retry in the past", attempt)
		}
	}
}

// Next must not loop once per attempt: the early return has to bound it.
func TestExponentialBackoff_HugeAttemptReturnsPromptly(t *testing.T) {
	done := make(chan time.Duration, 1)
	go func() { done <- ExponentialBackoff{Base: time.Second}.Next(math.MaxInt32) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Next(math.MaxInt32) did not return — the doubling loop is unbounded")
	}
}
