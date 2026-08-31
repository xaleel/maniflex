package maniflextest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/maniflextest"
)

var clockEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestClock_StartsAtTheGivenInstant(t *testing.T) {
	clock := maniflextest.NewClock(clockEpoch)

	if got := clock.Now(); !got.Equal(clockEpoch) {
		t.Errorf("Now() = %v, want %v", got, clockEpoch)
	}
}

func TestClock_AdvanceMovesTimeForward(t *testing.T) {
	clock := maniflextest.NewClock(clockEpoch)

	clock.Advance(90 * time.Minute)

	if want := clockEpoch.Add(90 * time.Minute); !clock.Now().Equal(want) {
		t.Errorf("after Advance: got %v, want %v", clock.Now(), want)
	}
}

func TestClock_SetReplacesTheInstant(t *testing.T) {
	clock := maniflextest.NewClock(clockEpoch)
	target := time.Date(2027, 6, 5, 4, 3, 2, 0, time.UTC)

	clock.Set(target)

	if !clock.Now().Equal(target) {
		t.Errorf("after Set: got %v, want %v", clock.Now(), target)
	}
}

// The method value is what gets handed to the framework, so it has to satisfy
// maniflex.Clock and stay bound to the clock it came from.
func TestClock_NowIsUsableAsAManiflexClock(t *testing.T) {
	clock := maniflextest.NewClock(clockEpoch)

	var injected maniflex.Clock = clock.Now
	clock.Advance(time.Hour)

	if want := clockEpoch.Add(time.Hour); !injected.Now().Equal(want) {
		t.Errorf("the injected clock read %v, want %v — the method value did not stay bound",
			injected.Now(), want)
	}
}

// A server under test reads the clock from handler goroutines while the test
// advances it, so the two must not race.
func TestClock_IsSafeForConcurrentUse(t *testing.T) {
	clock := maniflextest.NewClock(clockEpoch)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); clock.Advance(time.Second) }()
		wg.Add(1)
		go func() { defer wg.Done(); _ = clock.Now() }()
	}
	wg.Wait()

	if want := clockEpoch.Add(8 * time.Second); !clock.Now().Equal(want) {
		t.Errorf("after 8 concurrent advances: got %v, want %v", clock.Now(), want)
	}
}
