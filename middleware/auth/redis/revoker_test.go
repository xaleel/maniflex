package redis

import (
	"context"
	"time"
)

// revoker mirrors middleware/auth.Revoker.
//
// This module deliberately does not import maniflex — like the other satellite
// Redis modules, it satisfies a core interface structurally so that a consumer
// pulls in the Redis client without pulling in the framework, and the two
// version independently. The cost is that nothing checks the shape at compile
// time, so the interface is restated here and asserted below: a signature that
// drifts from the core's fails this module's build rather than an end user's.
//
// Keep in sync with middleware/auth.Revoker.
type revoker interface {
	RevokeToken(ctx context.Context, jti string, expiresAt time.Time) error
	IsTokenRevoked(ctx context.Context, jti string) (bool, error)
	RevokeUser(ctx context.Context, userID string, cutoff, retainUntil time.Time) error
	UserCutoff(ctx context.Context, userID string) (time.Time, error)
}

var _ revoker = (*Revoker)(nil)
