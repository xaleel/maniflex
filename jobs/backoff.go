package jobs

import (
	"math"
	"time"
)

// BackoffPolicy computes the delay before a job's next retry attempt.
type BackoffPolicy interface {
	Next(attempt int) time.Duration
}

// ExponentialBackoff doubles the delay on each attempt, capped at Max.
// This is the default policy when Job.Backoff is nil.
type ExponentialBackoff struct {
	// Base is the first attempt's delay. Zero means the default (1s).
	Base time.Duration
	// Max caps the delay. Zero means uncapped, in which case the delay
	// saturates at the largest representable Duration (~292 years) rather
	// than growing past it.
	Max time.Duration
}

// Next returns the delay before attempt's retry: Base doubled once per attempt,
// never past Max.
//
// The doubling saturates rather than overflowing. It used to be computed as
// float64(Base) * math.Pow(2, attempt) converted back to a Duration, and that
// conversion is implementation-defined once the value exceeds an int64 — on
// amd64 it yields math.MinInt64, a *negative* delay. Max did not save you: the
// cap is written `d > e.Max`, and a negative d is not greater than anything, so
// the overflowed value passed straight through. Callers add the result to
// time.Now(), so a negative delay put the next attempt in the past and the
// backoff collapsed into a hot retry loop against a dependency that was already
// failing — exactly when backing off matters. It needed no exotic input:
// Base 24h with Max 5m went negative at attempt 17 (audit JB-17).
func (e ExponentialBackoff) Next(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := e.Base
	if base <= 0 {
		base = time.Second
	}
	ceiling := e.Max
	if ceiling <= 0 {
		ceiling = math.MaxInt64
	}
	// Stop doubling as soon as the next double would pass the ceiling, so the
	// multiplication never has a chance to wrap. The early return also bounds
	// the loop at ~63 turns however large attempt is.
	d := base
	for range attempt {
		if d > ceiling/2 {
			return ceiling
		}
		d *= 2
	}
	if d > ceiling {
		return ceiling
	}
	return d
}

// FixedBackoff returns the same delay on every attempt.
type FixedBackoff struct {
	Delay time.Duration
}

func (f FixedBackoff) Next(_ int) time.Duration { return f.Delay }

// defaultBackoff is used when Job.Backoff is nil.
var defaultBackoff BackoffPolicy = ExponentialBackoff{Base: time.Second, Max: 5 * time.Minute}

// BackoffFor returns the effective backoff policy for j.
func BackoffFor(j Job) BackoffPolicy {
	if j.Backoff != nil {
		return j.Backoff
	}
	return defaultBackoff
}

// MaxRetryFor returns the effective max retry count for j.
func MaxRetryFor(j Job) int {
	if j.MaxRetry > 0 {
		return j.MaxRetry
	}
	return 3
}
