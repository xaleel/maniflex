package redis

import (
	"testing"
	"time"
)

// Audit AUTH-7: the key holds whole seconds, and this rounding decides a token
// minted in the same second as the revocation. Rounding down accepted it —
// including one minted just before the logout — while the in-memory and SQL
// stores refused it, so the same application answered differently depending on
// which blocklist it was wired to.
func TestUnixCeilRefusesTheCutoffsOwnSecond(t *testing.T) {
	const sec = 1700000000
	for _, tc := range []struct {
		name   string
		cutoff time.Time
		want   int64
	}{
		{"a remainder rounds up", time.Unix(sec, 500*int64(time.Millisecond)), sec + 1},
		{"one nanosecond is a remainder", time.Unix(sec, 1), sec + 1},
		{"exactly on the second stays", time.Unix(sec, 0), sec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unixCeil(tc.cutoff); got != tc.want {
				t.Errorf("unixCeil(%v) = %d, want %d", tc.cutoff, got, tc.want)
			}
		})
	}
}
