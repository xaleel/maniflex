package auth

// anonymous.go — AllowAnonymous, the per-route exemption that lets an
// authenticator tolerate a *missing* credential without being unregistered
// (audit AU-2).
//
// The problem it solves is one of shape rather than of capability. Pipeline
// filters are inclusion-only — ForModel and ForOperation name what a middleware
// covers, never what it skips — so the only way to make three routes of a
// hundred public was to scope the authenticator away from them, which means
// enumerating the ninety-seven that stay protected. That inverts the failure
// mode: a model registered next month is absent from the list, so it is covered
// by no auth registration at all, and nothing says so. The exemption is the
// smaller set and the safer thing to enumerate, because forgetting an entry
// there produces a 401 on the first request instead of an open endpoint.
//
// It also expresses something scoping cannot. A JWTAuth that does not run on a
// route leaves ctx.Auth nil even for a caller holding a valid token, so an
// endpoint cannot show more to a signed-in visitor than to a stranger — the
// draft-posts case. AllowAnonymous keeps the authenticator running and only
// forgives absence, so a token that *is* presented still lands in ctx.Auth.

import "github.com/xaleel/maniflex"

// anonymousOKKey marks a request whose route tolerates a missing credential.
//
// The marker rides on ctx.Set rather than a field on ServerContext because
// nothing outside this package needs to read it, and because it is not identity:
// it says what the route permits, not who the caller is. It cannot be reached
// from a request — ctx.Set has no HTTP surface — so the only way to set it is
// for the app to have registered AllowAnonymous on that route itself.
const anonymousOKKey = "maniflex.auth.anonymous_ok"

// AllowAnonymous marks the routes it is registered for as tolerating a request
// that carries no credential at all. Register it BEFORE the authenticator, and
// scope it with the usual filters:
//
//	server.Pipeline.Auth.Register(auth.AllowAnonymous(),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpList, maniflex.OpRead))
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret))
//
// JWTAuth and JWKSAuth honour it. Exactly one thing changes: a request with no
// token is served with ctx.Auth left nil, instead of answering 401.
//
// Nothing else moves. A credential that was presented and failed — expired,
// wrongly signed, revoked, malformed, or framed without its scheme — is still
// 401 here, as it is everywhere else. That distinction is the whole security
// content of this middleware: degrading a bad token to anonymous would turn an
// expired session into a silent change of permissions, and would let a caller
// shed a restrictive role by corrupting one byte of their own token.
//
// ctx.Auth stays nil for the anonymous caller rather than being filled with a
// blank principal, so every `ctx.Auth == nil` test in the framework keeps
// meaning what it means — including the ones that fail closed on it, like the
// per-actor scope on the job-status routes.
//
// Ordering is by registration within a step, so an AllowAnonymous registered
// after the authenticator sets the marker too late to be read. That mistake
// fails closed: the route answers 401 on the first anonymous request.
func AllowAnonymous() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		ctx.Set(anonymousOKKey, true)
		return next()
	}
}

// anonymousAllowed reports whether AllowAnonymous ran for this request.
func anonymousAllowed(ctx *maniflex.ServerContext) bool {
	v, ok := ctx.Get(anonymousOKKey)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}
