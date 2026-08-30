package auth

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/xaleel/maniflex"
)

// RequireScope returns 403 unless ctx.Auth holds every one of the given OAuth2
// scopes. Must be registered After an auth middleware that populates ctx.Auth;
// JWTAuth fills Scopes from JWTOptions.ScopesClaim (default "scope").
//
// Note the semantics differ from RequireRole, which passes on any one of the
// roles it is given. A role names who the caller is, so holding one of several
// is the usual question. A scope names a grant the caller was issued, so an
// endpoint that both reads and writes needs both grants — not either. Use
// RequireAnyScope where one of several grants is genuinely enough.
//
// Scopes match exactly: a "read:*" grant does not satisfy "read:posts", because
// the framework cannot know what delimits a given issuer's scope hierarchy.
// Express a hierarchy with Enforce and a Policy.
//
//	server.Pipeline.Auth.Register(auth.RequireScope("posts:read", "posts:write"),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpUpdate),
//	)
func RequireScope(scopes ...string) maniflex.MiddlewareFunc {
	required := checkedScopes("RequireScope", scopes)
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth == nil {
			abortUnauthenticated(ctx)
			return nil
		}
		var missing []string
		for _, want := range required {
			if !slices.Contains(ctx.Auth.Scopes, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			// Only the shortfall is named. A caller granted three of four
			// scopes needs to know which one to go and ask for.
			ctx.Abort(http.StatusForbidden, "FORBIDDEN",
				fmt.Sprintf("missing required scope: %s", strings.Join(missing, ", ")))
			return nil
		}
		return next()
	}
}

// RequireAnyScope returns 403 unless ctx.Auth holds at least one of the given
// OAuth2 scopes — the RequireRole semantics, for endpoints where several grants
// are each sufficient on their own.
//
//	server.Pipeline.Auth.Register(auth.RequireAnyScope("posts:write", "admin"),
//	    maniflex.ForModel("Post"),
//	)
func RequireAnyScope(scopes ...string) maniflex.MiddlewareFunc {
	accepted := checkedScopes("RequireAnyScope", scopes)
	scopeSet := make(map[string]bool, len(accepted))
	for _, s := range accepted {
		scopeSet[s] = true
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth == nil {
			abortUnauthenticated(ctx)
			return nil
		}
		for _, held := range ctx.Auth.Scopes {
			if scopeSet[held] {
				return next()
			}
		}
		ctx.Abort(http.StatusForbidden, "FORBIDDEN",
			fmt.Sprintf("one of the following scopes is required: %s", strings.Join(accepted, ", ")))
		return nil
	}
}

// checkedScopes copies the caller's slice so a later append cannot mutate the
// guard, and refuses an empty one: under RequireScope's all-of semantics a
// zero-length requirement is vacuously satisfied, so the guard would admit
// every request while reading as though it protected the route.
func checkedScopes(name string, scopes []string) []string {
	if len(scopes) == 0 {
		panic("auth." + name + ": at least one scope is required; " +
			"with none the guard would admit every request")
	}
	return slices.Clone(scopes)
}

func abortUnauthenticated(ctx *maniflex.ServerContext) {
	ctx.Abort(http.StatusForbidden, "FORBIDDEN", "authentication required")
}
