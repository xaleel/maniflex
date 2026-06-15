package workflow

import (
	"fmt"

	"github.com/xaleel/maniflex"
)

// Guard gates a state transition on something beyond the from/to pair —
// typically the caller's roles, but it may inspect any aspect of the
// request. Returning a non-nil error rejects the transition with the
// error's message as the 422 detail.
type Guard interface {
	Check(ctx *maniflex.ServerContext, from, to string) error
}

// GuardFunc adapts a plain function to the Guard interface.
type GuardFunc func(ctx *maniflex.ServerContext, from, to string) error

// Check implements Guard.
func (f GuardFunc) Check(ctx *maniflex.ServerContext, from, to string) error {
	return f(ctx, from, to)
}

// RequireRole accepts the transition only if the caller holds any one of
// the listed roles (OR-semantics, matching maniflex.HasRole). With no roles
// passed, the guard always rejects — that protects against accidentally
// declaring an unguarded "require" rule.
func RequireRole(roles ...string) Guard {
	return GuardFunc(func(ctx *maniflex.ServerContext, from, to string) error {
		for _, r := range roles {
			if ctx.HasRole(r) {
				return nil
			}
		}
		return fmt.Errorf(
			"role %v required for transition %q → %q", roles, from, to)
	})
}
