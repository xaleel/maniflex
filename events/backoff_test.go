package events

// Audit EV-13: the Kafka and Redis consume loops retried a failed broker read
// with a bare `time.Sleep(time.Second)`. Three problems, all of which only
// show up when something is already wrong:
//
//   - No growth. A broker down for an hour is polled 3600 times per consumer,
//     per partition or stream, on every replica.
//   - No jitter. Every consumer in the fleet lost the broker at the same
//     instant, so they all retry on the same second forever — including the
//     moment it comes back, which is the worst possible time to synchronise.
//   - time.Sleep ignores the context. Shutdown had to wait out the full
//     second per consumer, after cancellation had already been requested.
//
//	go test ./events/ -run TestReadBackoff

import (
	"context"
	"testing"
	"time"
)

func TestReadBackoff_GrowsAndCaps(t *testing.T) {
	bo := ReadBackoff{Min: 100 * time.Millisecond, Max: 3200 * time.Millisecond}

	// Jitter puts each delay in [d/2, d), so assert on the band rather than an
	// exact value — the property that matters is that the band moves up.
	want := []time.Duration{100, 200, 400, 800, 1600, 3200, 3200, 3200}
	for i, base := range want {
		n, d, _ := bo.Next()
		if n != i+1 {
			t.Errorf("attempt %d: got n=%d", i+1, n)
		}
		lo, hi := base*time.Millisecond/2, base*time.Millisecond
		if d < lo || d >= hi {
			t.Errorf("attempt %d: delay %v outside [%v, %v)", i+1, d, lo, hi)
		}
	}
}

// The old code was a fixed 1s. If the delay never grew, a persistent outage
// would cost the same request rate at minute 60 as at second 1.
func TestReadBackoff_LaterAttemptsAreSlowerThanEarlyOnes(t *testing.T) {
	bo := ReadBackoff{Min: 100 * time.Millisecond, Max: 30 * time.Second}

	_, first, _ := bo.Next()
	var last time.Duration
	for range 10 {
		_, last, _ = bo.Next()
	}
	if last <= first*10 {
		t.Errorf("delay barely grew: attempt 1 = %v, attempt 11 = %v", first, last)
	}
}

// Jitter is the whole reason a recovering broker is not hit by the entire
// fleet on the same tick. Two independent backoffs at the same attempt must
// not agree on their delay.
func TestReadBackoff_Jitters(t *testing.T) {
	seen := make(map[time.Duration]int)
	for range 50 {
		bo := ReadBackoff{Min: time.Second, Max: time.Minute}
		for range 5 {
			bo.Next()
		}
		_, d, _ := bo.Next()
		seen[d]++
	}
	if len(seen) < 25 {
		t.Errorf("only %d distinct delays across 50 consumers: the fleet retries in lockstep", len(seen))
	}
}

// Jitter must not undershoot into a busy-loop. Full jitter (rand in [0, d))
// would allow a near-zero wait, defeating the point of backing off at all.
func TestReadBackoff_NeverNearZero(t *testing.T) {
	bo := ReadBackoff{Min: 200 * time.Millisecond, Max: time.Minute}
	for range 200 {
		_, d, _ := bo.Next()
		if d < 100*time.Millisecond {
			t.Fatalf("delay %v is below half of Min: the loop would hammer the broker", d)
		}
		bo.Reset()
	}
}

// escalate marks the transition from "transient blip" to "outage" exactly
// once, so the ERROR is findable and the retries after it stay at WARN.
func TestReadBackoff_EscalatesOnceAtCap(t *testing.T) {
	bo := ReadBackoff{Min: 100 * time.Millisecond, Max: 400 * time.Millisecond}

	var escalations, atCapAttempt int
	for i := range 10 {
		_, _, esc := bo.Next()
		if esc {
			escalations++
			atCapAttempt = i + 1
		}
	}
	if escalations != 1 {
		t.Errorf("escalated %d times, want exactly 1", escalations)
	}
	if atCapAttempt != 3 { // 100 → 200 → 400 = Max
		t.Errorf("escalated on attempt %d, want 3 (the first to reach Max)", atCapAttempt)
	}
}

func TestReadBackoff_ResetRestoresBothGrowthAndEscalation(t *testing.T) {
	bo := ReadBackoff{Min: 100 * time.Millisecond, Max: 400 * time.Millisecond}
	for range 5 {
		bo.Next()
	}
	bo.Reset()

	n, d, esc := bo.Next()
	if n != 1 {
		t.Errorf("attempt = %d after Reset, want 1", n)
	}
	if d >= 100*time.Millisecond {
		t.Errorf("delay %v after Reset, want back down near Min", d)
	}
	if esc {
		t.Error("escalated on the first attempt after Reset")
	}

	// A broker that recovers and fails again later deserves a fresh ERROR.
	var escalations int
	for range 5 {
		if _, _, e := bo.Next(); e {
			escalations++
		}
	}
	if escalations != 1 {
		t.Errorf("escalated %d times after Reset, want 1", escalations)
	}
}

// The reason time.Sleep had to go: cancellation must be observed immediately,
// not after the remaining delay elapses.
func TestReadBackoff_WaitReturnsOnCancel(t *testing.T) {
	var bo ReadBackoff
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if bo.Wait(ctx, 10*time.Second) {
		t.Error("Wait reported success after its context was cancelled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait blocked %v after cancellation: shutdown stalls behind the backoff", elapsed)
	}
}

func TestReadBackoff_WaitCompletesNormally(t *testing.T) {
	var bo ReadBackoff
	if !bo.Wait(context.Background(), time.Millisecond) {
		t.Error("Wait reported failure with a live context")
	}
}

// The zero value is what an adapter gets by writing `var bo events.ReadBackoff`,
// so it has to be usable without any configuration.
func TestReadBackoff_ZeroValueUsesDefaults(t *testing.T) {
	var bo ReadBackoff
	_, d, _ := bo.Next()
	if d < DefaultReadBackoffMin/2 || d >= DefaultReadBackoffMin {
		t.Errorf("first delay %v, want the %v default band", d, DefaultReadBackoffMin)
	}

	for range 30 {
		bo.Next()
	}
	_, d, _ = bo.Next()
	if d >= DefaultReadBackoffMax {
		t.Errorf("delay %v exceeded the %v cap", d, DefaultReadBackoffMax)
	}
}

// Min > Max is a caller error that must not produce a nonsense delay or a
// division that panics.
func TestReadBackoff_InvertedBoundsDoNotPanic(t *testing.T) {
	bo := ReadBackoff{Min: time.Minute, Max: time.Second}
	_, d, _ := bo.Next()
	if d < 30*time.Second || d >= time.Minute {
		t.Errorf("delay %v, want Min honoured when Max is below it", d)
	}
}
