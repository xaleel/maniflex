package sql

import (
	"testing"
	"time"
)

// Dequeue is a poll loop, so the expired-lease sweep is throttled rather than
// run on every call — an unthrottled sweep takes the SQLite write lock once per
// poll to update nothing. See tests/e2e/jobs_sql_reclaim_test.go for the sweep's
// behaviour itself.
func TestShouldReclaim_ThrottlesToATenthOfTheLease(t *testing.T) {
	q := &Queue{lease: time.Minute} // → a 6s window
	t0 := time.Now()

	if !q.shouldReclaim(t0) {
		t.Fatal("the first call was throttled; a fresh queue must sweep before it claims")
	}
	if q.shouldReclaim(t0.Add(time.Second)) {
		t.Fatal("a second sweep ran 1s later, inside the 6s window")
	}
	if q.shouldReclaim(t0.Add(5 * time.Second)) {
		t.Fatal("a sweep ran 5s later, still inside the 6s window")
	}
	if !q.shouldReclaim(t0.Add(7 * time.Second)) {
		t.Fatal("no sweep ran after the window elapsed — expired leases would never be collected")
	}
}

// A deliberately long visibility timeout must not park the sweep for hours.
func TestShouldReclaim_LongLeaseIsClampedToAMinute(t *testing.T) {
	q := &Queue{lease: 24 * time.Hour} // /10 would be 2.4h
	t0 := time.Now()

	if !q.shouldReclaim(t0) {
		t.Fatal("first call was throttled")
	}
	if q.shouldReclaim(t0.Add(30 * time.Second)) {
		t.Fatal("a sweep ran 30s later, inside the 1m clamp")
	}
	if !q.shouldReclaim(t0.Add(90 * time.Second)) {
		t.Fatal("a 24h lease parked the sweep past the 1m clamp")
	}
}

// A short lease is used by tests and by anyone wanting fast crash recovery; the
// window has to follow it down rather than sitting at some floor.
func TestShouldReclaim_ShortLeaseSweepsPromptly(t *testing.T) {
	q := &Queue{lease: 50 * time.Millisecond} // → a 5ms window
	t0 := time.Now()

	if !q.shouldReclaim(t0) {
		t.Fatal("first call was throttled")
	}
	if !q.shouldReclaim(t0.Add(10 * time.Millisecond)) {
		t.Fatal("a 50ms lease still throttled a sweep 10ms later")
	}
}
