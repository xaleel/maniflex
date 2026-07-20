package events

import (
	"context"
	"math/rand/v2"
	"time"
)

// Default bounds for ReadBackoff. The floor is short enough that a one-off
// blip costs almost nothing; the ceiling is the point past which waiting
// longer stops helping, since a broker that has been down for 30s is an
// outage rather than a hiccup and the loop should keep checking at a steady,
// cheap rate.
const (
	DefaultReadBackoffMin = 100 * time.Millisecond
	DefaultReadBackoffMax = 30 * time.Second
)

// ReadBackoff paces an adapter's consume loop when reading from the broker
// fails. It grows the delay exponentially between Min and Max and applies
// jitter, so a fleet of consumers that all lost the broker at the same instant
// does not reconnect in lockstep.
//
// A read loop retries forever by design — there is no attempt limit, because
// the alternative to retrying is a consumer that silently stops consuming.
// The backoff is what keeps "forever" affordable.
//
// The zero value is usable and applies the defaults above. It is not safe for
// concurrent use; give each consume loop its own.
//
//	var bo events.ReadBackoff
//	for {
//	    msg, err := read(ctx)
//	    if err != nil {
//	        n, d, escalate := bo.Next()
//	        // ...log at WARN, or at ERROR when escalate is true...
//	        if !bo.Wait(ctx, d) {
//	            return
//	        }
//	        continue
//	    }
//	    bo.Reset()
//	    handle(msg)
//	}
type ReadBackoff struct {
	// Min is the delay after the first failure. Zero means DefaultReadBackoffMin.
	Min time.Duration
	// Max caps the delay. Zero means DefaultReadBackoffMax.
	Max time.Duration

	n        int
	capNoted bool
}

func (b *ReadBackoff) bounds() (minDelay, maxDelay time.Duration) {
	minDelay, maxDelay = b.Min, b.Max
	if minDelay <= 0 {
		minDelay = DefaultReadBackoffMin
	}
	if maxDelay <= 0 {
		maxDelay = DefaultReadBackoffMax
	}
	if maxDelay < minDelay {
		maxDelay = minDelay
	}
	return minDelay, maxDelay
}

// Next advances to the next attempt and returns its number (1 for the first
// failure), the delay to wait, and whether this is the first attempt to reach
// Max. Callers use escalate to log once at ERROR when a run of failures stops
// looking transient, without turning every subsequent retry into an ERROR.
//
// The delay is jittered within the upper half of the computed interval: full
// jitter would allow a near-zero wait, which is the one outcome a loop meant
// to stop hammering a struggling broker cannot afford.
func (b *ReadBackoff) Next() (attempt int, delay time.Duration, escalate bool) {
	minDelay, maxDelay := b.bounds()
	b.n++

	d := minDelay
	// Shift rather than math.Pow, and stop early: at 100ms a base of 1<<40 is
	// already centuries, and the shift itself would overflow past 63.
	for i := 1; i < b.n && d < maxDelay; i++ {
		d *= 2
	}
	atCap := d >= maxDelay
	if atCap {
		d = maxDelay
	}

	half := d / 2
	if half > 0 {
		d = half + time.Duration(rand.Int64N(int64(half)))
	}

	if atCap && !b.capNoted {
		b.capNoted = true
		escalate = true
	}
	return b.n, d, escalate
}

// Wait blocks for d, reporting false if ctx ended first. A read loop must
// treat false as "stop": shutdown was requested, and unlike time.Sleep this
// does not make the caller wait out the remaining delay before noticing.
func (b *ReadBackoff) Wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Reset returns the backoff to its initial state. Call it after every
// successful read, so an unrelated failure hours later starts from Min rather
// than inheriting a stale delay — and so a recovered broker is allowed to
// escalate again if it fails a second time.
func (b *ReadBackoff) Reset() {
	b.n = 0
	b.capNoted = false
}
