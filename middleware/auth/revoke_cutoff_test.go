package auth

import (
	"context"
	"testing"
	"time"
)

// Audit AUTH-7: iat is normally a whole second, so a token minted in the same
// second as a "log out everywhere" cannot be placed either side of it. The
// check refuses that second — deliberately, since the alternative honours a
// token minted moments before the logout — which also killed the replacement
// token an app issues straight after a password change.

func TestIssuedAtKeepsTheFraction(t *testing.T) {
	const sec = 1700000000
	base := time.Unix(sec, 0)
	for _, tc := range []struct {
		name string
		iat  float64
		want time.Duration // offset from base
	}{
		{"whole second", sec, 0},
		{"half", sec + 0.5, 500 * time.Millisecond},
		{"after a .500 cutoff", sec + 0.7, 700 * time.Millisecond},
		{"before it", sec + 0.3, 300 * time.Millisecond},
		{"milliseconds", sec + 0.001, time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := issuedAt(tc.iat).Sub(base)
			// A float64 near 1.7e9 cannot carry nanoseconds; microseconds is far
			// finer than anything this decides.
			if d := got - tc.want; d > time.Microsecond || d < -time.Microsecond {
				t.Errorf("iat %.3f → base%+v, want base%+v", tc.iat, got, tc.want)
			}
		})
	}
}

// The whole point of the wait: what it returns after is a second that the
// check can place tokens against.
func TestWaitOutAmbiguousSecondCrossesTheBoundary(t *testing.T) {
	cutoff := time.Now()
	if cutoff.Nanosecond() == 0 {
		t.Skip("cutoff landed exactly on a second; nothing is ambiguous")
	}
	waitOutAmbiguousSecond(context.Background(), cutoff)
	if !time.Now().After(cutoff.Truncate(time.Second).Add(time.Second)) {
		t.Error("returned while the cutoff's second was still running")
	}
}

// A cutoff exactly on a second is not ambiguous — a token from that second is
// not *before* it, so it is already accepted — and waiting would be a second
// spent for nothing.
func TestWaitOutAmbiguousSecondSkipsWholeSeconds(t *testing.T) {
	start := time.Now()
	waitOutAmbiguousSecond(context.Background(), time.Unix(1700000000, 0))
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("waited %v for a cutoff with no remainder", elapsed)
	}
}

// The revocation is already stored when the wait starts, so an abandoned
// request must not hold the handler for the rest of the second.
func TestWaitOutAmbiguousSecondReturnsWhenTheCallerIsGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	waitOutAmbiguousSecond(ctx, time.Now().Add(-500*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("waited %v after the request was abandoned", elapsed)
	}
}
