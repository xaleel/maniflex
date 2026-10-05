package auth

// firstof.go — FirstOf, the combinator that accepts any one of several
// credentials on the same route (audit AUTH-12).
//
// The catalogue promised this for years and the code never did it. Both shipped
// authenticators abort 401 when their own header is absent, so registering
// APIKeyAuth and JWTAuth on the same step demanded *both* credentials and locked
// out every single-credential client — and when both were presented, the last
// registration overwrote the first's principal rather than the documented "first
// match wins".
//
// The reason it cannot be fixed by making the authenticators yield to an
// already-installed ctx.Auth is written out at trustedPrincipal: this middleware
// deliberately overwrites a principal an earlier Auth middleware set, because an
// app whose earlier middleware trusts a header would otherwise silently start
// keeping it. So the composition has to be expressed, not inferred.

import (
	"net/http"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/internal/accessdecision"
)

// credentialOptionalKey marks a request whose authenticators are being run as
// alternatives, so a *missing* credential is not yet a refusal — a sibling may
// supply one, and FirstOf answers 401 itself if none does.
//
// Deliberately NOT anonymousOKKey, which would have been one line cheaper.
// That marker means "this route is served without a credential at all", and
// APIKeyAuth does not honour it: teaching it to would change what
// AllowAnonymous means for every app that already registers an API key, opening
// an anonymous path on routes whose operator asked only for optional JWT. This
// marker is set by FirstOf alone, for the duration of its own members, and
// cleared before the rest of the pipeline runs.
const credentialOptionalKey = "maniflex.auth.credential_optional"

// credentialOptional reports whether a missing credential should be deferred to
// a sibling authenticator rather than refused.
func credentialOptional(ctx *maniflex.ServerContext) bool {
	v, ok := ctx.Get(credentialOptionalKey)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// FirstOf accepts any one of several credentials on the same route: it runs the
// given authenticators in order and stops at the first that establishes an
// identity.
//
//	server.Pipeline.Auth.Register(auth.FirstOf(
//	    auth.APIKeyAuth("X-API-Key", serviceKeys...),
//	    auth.JWTAuth(secret),
//	))
//
// That serves the shape most APIs reach for eventually — service accounts
// authenticate with a long-lived key, people authenticate with a token, and both
// use the same endpoints — without scoping either authenticator away from
// routes, which inverts the failure mode: a model added next month is absent
// from the inclusion list and so is covered by no authentication at all.
//
// A **missing** credential is what gets deferred, and only that. A credential
// that was presented and failed — an unknown API key, an expired or wrongly
// signed token, one framed without its scheme — refuses the request there and
// then, with that authenticator's own message, and the remaining alternatives
// are not tried. This is the whole security content of the combinator: falling
// through to the next authenticator on a *bad* credential would let a caller
// shed a restrictive identity by corrupting one byte of it, and would make a
// revoked token equivalent to no token.
//
// When no member saw a credential, FirstOf answers 401 — unless the route also
// carries [AllowAnonymous], in which case it is served with ctx.Auth left nil,
// exactly as a single authenticator under that marker would serve it.
//
// Order decides precedence only among credentials a caller presents *together*.
// A request carrying both an API key and a token is authenticated by whichever
// member is listed first, and the later ones do not run — which is the "first
// match wins" the catalogue described and the plain registration never gave.
//
// Members must be authenticators: middleware that establishes ctx.Auth from a
// credential on the request. Wrapping a check that decides something else —
// RequireRole, CSRF — is not meaningful, because FirstOf stops at the first
// member that sets a principal and treats the rest as untried.
func FirstOf(authenticators ...maniflex.MiddlewareFunc) maniflex.MiddlewareFunc {
	if len(authenticators) < 2 {
		panic("auth.FirstOf: needs at least two authenticators; one alternative is " +
			"just that authenticator, and none authenticates nothing")
	}
	panicIfNothingDecides(authenticators)

	fn := func(ctx *maniflex.ServerContext, next func() error) error {
		// An Execute carrying a principal has no headers for any member to read,
		// so the members would all report "absent" and FirstOf would 401 a call
		// whose identity is already established. Same rule, and the same reason,
		// as each member applies on its own (audit AUTH-11).
		if trustedPrincipal(ctx) {
			return next()
		}

		// Pointer identity rather than a nil check: a principal installed by some
		// earlier Auth middleware must not read as "a member matched", or FirstOf
		// would vouch for an identity none of its members established.
		entry := ctx.Auth

		ctx.Set(credentialOptionalKey, true)
		matched, refused := false, false
		for _, m := range authenticators {
			// The members are run with a no-op continuation: FirstOf decides what
			// happens after them, and a member calling through to the real next()
			// would run the rest of the pipeline once per alternative.
			if err := m(ctx, func() error { return nil }); err != nil {
				ctx.Set(credentialOptionalKey, false)
				return err
			}
			if ctx.Response != nil && ctx.Response.StatusCode >= http.StatusBadRequest {
				refused = true // presented and failed: that refusal stands
				break
			}
			if ctx.Auth != entry {
				matched = true
				break
			}
		}
		// Cleared before the rest of the pipeline, so an authenticator registered
		// outside FirstOf does not inherit the tolerance.
		ctx.Set(credentialOptionalKey, false)

		switch {
		case refused:
			return nil
		case matched:
			return next()
		case anonymousAllowed(ctx):
			return next()
		}
		ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
			"no recognised credential was presented")
		return nil
	}

	return fn
}

// panicIfNothingDecides refuses a FirstOf whose every member is marked as
// deciding nothing — FirstOf(CSRF(), AllowAnonymous()) and the like. Such a
// composition authenticates no one: no member installs a principal, so FirstOf
// answers 401 to every request that is not anonymous, and it would otherwise
// satisfy ValidateProduction's access check that neither member satisfies alone
// (audit AUTH-6).
//
// Caught at construction rather than propagated through the marker, and that is
// forced rather than chosen. accessdecision keys on a function's code pointer,
// which every closure from one literal shares — so marking *this* FirstOf would
// mark every FirstOf in the process, and one nonsense composition in a test or
// a dead code path would quietly un-count every real one. Reading the members
// is sound, because each arrives from its own constructor's literal; only
// writing a per-instance verdict is not. A panic needs no per-instance state
// and reports the mistake at boot instead of at audit time.
func panicIfNothingDecides(authenticators []maniflex.MiddlewareFunc) {
	for _, m := range authenticators {
		if !accessdecision.IsNotADecision(m) {
			return
		}
	}
	panic("auth.FirstOf: every member is transport-only middleware that decides " +
		"nothing about who may call a route (auth.CSRF, auth.AllowAnonymous), so " +
		"this composition authenticates no one; list at least one authenticator")
}
