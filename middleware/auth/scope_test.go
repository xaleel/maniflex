package auth_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
)

// scoped builds an authenticated principal carrying the given OAuth2 scopes.
func scoped(scopes ...string) *maniflex.AuthInfo {
	return &maniflex.AuthInfo{UserID: "u1", Scopes: scopes}
}

// runGuard applies mw to a bare context and reports whether the request reached
// the next step. A nil principal models an anonymous request.
func runGuard(t *testing.T, mw maniflex.MiddlewareFunc, principal *maniflex.AuthInfo) (bool, *maniflex.ServerContext) {
	t.Helper()
	ctx := &maniflex.ServerContext{Auth: principal}
	reached := false
	if err := mw(ctx, func() error { reached = true; return nil }); err != nil {
		t.Fatalf("middleware returned an error: %v", err)
	}
	return reached, ctx
}

func assertForbidden(t *testing.T, ctx *maniflex.ServerContext, wantInMessage string) {
	t.Helper()
	if ctx.Response == nil {
		t.Fatal("expected the request to be aborted, but it was not")
	}
	if ctx.Response.StatusCode != http.StatusForbidden {
		t.Errorf("status: want %d, got %d", http.StatusForbidden, ctx.Response.StatusCode)
	}
	if ctx.Response.Error == nil {
		t.Fatal("aborted without an error body")
	}
	if ctx.Response.Error.Code != "FORBIDDEN" {
		t.Errorf("error code: want FORBIDDEN, got %q", ctx.Response.Error.Code)
	}
	if wantInMessage != "" && !strings.Contains(ctx.Response.Error.Message, wantInMessage) {
		t.Errorf("message %q does not mention %q", ctx.Response.Error.Message, wantInMessage)
	}
}

// RequireScope is AND, not OR: an OAuth2 caller needing read and write access
// must hold both grants. This is the semantics that differs from RequireRole,
// so it is the one worth pinning first.
func TestRequireScope_RequiresEveryListedScope(t *testing.T) {
	mw := auth.RequireScope("read:posts", "write:posts")

	reached, ctx := runGuard(t, mw, scoped("read:posts", "write:posts", "read:users"))
	if !reached {
		t.Fatalf("a principal holding both scopes was refused: %+v", ctx.Response)
	}
}

func TestRequireScope_RefusesWhenOneScopeIsMissing(t *testing.T) {
	mw := auth.RequireScope("read:posts", "write:posts")

	reached, ctx := runGuard(t, mw, scoped("read:posts"))
	if reached {
		t.Fatal("a principal holding only one of two required scopes was allowed through")
	}
	assertForbidden(t, ctx, "write:posts")
}

// The refusal names the scope that was missing, not the whole required set: a
// caller granted three of four scopes needs to know which one to ask for.
func TestRequireScope_NamesOnlyTheMissingScopes(t *testing.T) {
	mw := auth.RequireScope("read:posts", "write:posts", "delete:posts")

	_, ctx := runGuard(t, mw, scoped("read:posts"))
	assertForbidden(t, ctx, "write:posts")
	if strings.Contains(ctx.Response.Error.Message, "read:posts") {
		t.Errorf("message %q names a scope the caller already holds", ctx.Response.Error.Message)
	}
}

func TestRequireAnyScope_AllowsWhenOneScopeMatches(t *testing.T) {
	mw := auth.RequireAnyScope("read:posts", "write:posts")

	reached, ctx := runGuard(t, mw, scoped("write:posts"))
	if !reached {
		t.Fatalf("a principal holding one accepted scope was refused: %+v", ctx.Response)
	}
}

func TestRequireAnyScope_RefusesWhenNoScopeMatches(t *testing.T) {
	mw := auth.RequireAnyScope("read:posts", "write:posts")

	reached, ctx := runGuard(t, mw, scoped("read:users"))
	if reached {
		t.Fatal("a principal holding none of the accepted scopes was allowed through")
	}
	assertForbidden(t, ctx, "read:posts")
}

// Both guards must refuse an unauthenticated request rather than panicking on a
// nil principal — they run on Pipeline.Auth, where an earlier middleware may
// have declined to populate ctx.Auth.
func TestRequireScope_RefusesAnonymousRequests(t *testing.T) {
	for name, mw := range map[string]maniflex.MiddlewareFunc{
		"RequireScope":    auth.RequireScope("read:posts"),
		"RequireAnyScope": auth.RequireAnyScope("read:posts"),
	} {
		t.Run(name, func(t *testing.T) {
			reached, ctx := runGuard(t, mw, nil)
			if reached {
				t.Fatal("an anonymous request was allowed through")
			}
			assertForbidden(t, ctx, "authentication required")
		})
	}
}

// Scopes match exactly. A wildcard grant is not expanded, because the framework
// does not know whether ":" or "/" delimits a caller's scope hierarchy — express
// that with auth.Enforce and a Policy instead.
func TestRequireScope_MatchesExactlyAndDoesNotExpandWildcards(t *testing.T) {
	reached, ctx := runGuard(t, auth.RequireScope("read:posts"), scoped("read:*"))
	if reached {
		t.Fatal("a wildcard grant satisfied an exact scope requirement")
	}
	assertForbidden(t, ctx, "read:posts")
}

// A guard constructed with no scopes would admit every request under AND
// semantics — a middleware named "Require" that requires nothing. That is a
// wiring mistake, so it fails at construction like JWTAuth's empty secret.
func TestRequireScope_PanicsWhenNoScopeIsNamed(t *testing.T) {
	assertPanics(t, "RequireScope()", func() { auth.RequireScope() })
	assertPanics(t, "RequireAnyScope()", func() { auth.RequireAnyScope() })
}
