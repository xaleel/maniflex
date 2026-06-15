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
	Base time.Duration
	Max  time.Duration
}

func (e ExponentialBackoff) Next(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := time.Duration(float64(e.Base) * math.Pow(2, float64(attempt)))
	if e.Max > 0 && d > e.Max {
		d = e.Max
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
