# Production Validation

Development defaults favor a short feedback loop: generated routes are public
until Auth middleware is registered, `QueryTimeout` is unlimited, and
AutoMigrate is enabled. Before deployment, audit the fully assembled server:

```go
cfg := maniflex.Config{
    Strict:             true,
    DisableAutoMigrate: true,
    QueryTimeout:       30 * time.Second,
}
server := maniflex.New(cfg)
server.MustRegister(User{}, Order{})
server.Pipeline.Auth.Register(auth.JWTAuth(publicKey, auth.JWTOptions{}))

if err := server.ValidateProduction(); err != nil {
    log.Fatal(err)
}
log.Fatal(server.Start())
```

Call `ValidateProduction` after registering models, middleware, actions, global
search, and documentation, but before `Start` or `Handler`. It reports every
problem together and changes no runtime behavior.

## What it requires

- `Config.Strict` is enabled.
- `Config.QueryTimeout` is positive.
- `Config.MaxConcurrentRequests` is positive, so a burst is refused rather than
  queued on the database pool.
- Every global and per-model effective `QueryLimits` field remains positively
  bounded, as does global search's `MaxLimit` when search is mounted.
- AutoMigrate is disabled when models are registered.
- Every generated model operation has matching `Pipeline.Auth` middleware or an
  explicit public declaration.
- Standalone files, custom actions, and global search each have a protected or
  explicitly public access decision.

### What counts as an access decision

Any `Pipeline.Auth` middleware that applies to the route counts — an
authenticator, `RequireRole` and the other `Require*` checks, `AllowPublicRead`,
`BlockOperation`, and any middleware you write yourself, since the validator
cannot see inside it.

Two of the framework's do **not** count, because they decide nothing about who
may call the route:

- `auth.CSRF` passes every safe method untouched, and on writes checks only that
  a cookie matches a header — which any client that isn't a browser sets for
  itself.
- `auth.AllowAnonymous` only tells an authenticator that a missing credential is
  acceptable. With no authenticator registered, it lets everyone through.

Either one on its own therefore leaves the route uncovered, and the report says
so. Register them alongside an authenticator, as they are meant to be used. A
middleware of your own that wraps one of them counts, like any other middleware
you write.

`BlockOperation` counts for every operation it applies to, not just the ones it
refuses — so `BlockOperation(OpDelete)` alone still satisfies the check for the
model's other operations. Pair it with an authenticator, or declare the other
operations with `AllowPublic`.

### What it cannot check

The audit asks whether a decision was made, not how strong the authenticator
making it is. A middleware is an opaque closure here, so nothing in
`ValidateProduction` can see that a `JWKSAuth` has no `Audience`, that
`AllowNoExpiry` is set, or how large a `ClockSkew` is. Those are reported where
they are configured instead — `JWKSAuth` warns without an `Audience`, a
`ClockSkew` over five minutes warns and a negative one panics, a plaintext JWKS
URL warns, and an empty HMAC secret panics. Read the startup log as part of a
deployment check; a clean `ValidateProduction` does not mean the authenticator
is configured well.

It also runs every registry check `Start` runs, so one call reports the whole
startup posture rather than only the part specific to production:

- Encrypted `unique` fields have a blind-index key, so their uniqueness digests
  can still be re-derived after a key rotation.
- Fields tagged `file_acl:signed` have a `FileStorage` that can mint a
  time-limited URL, rather than degrading to a permanent one.
- Every model field has a Go type the OpenAPI generator can describe.
- Relations, `lock_scope` declarations, and `Pipeline` middleware wiring resolve.

Several of those are findings `Config.Strict` turns fatal, and production
validation requires `Strict`, so they surface here rather than at the first boot.

Framework outbound calls made through `integration.Caller` already have bounded
timeout, retry, and response-size defaults. The validator cannot inspect
arbitrary `http.Client` instances created by application code.

Proxy-header resolution remains off by default, which is safe. `TrustedProxies`
names the peers whose forwarding headers may be believed, and the validator can
check that the entries parse but not that they describe your actual topology.

`TrustProxyHeaders: true` with no `TrustedProxies` is the legacy allowlist-free
mode — the explicit assertion that the service sits behind a proxy which replaces
client-supplied forwarding headers itself. It warns at startup and, because
`Config.Strict` is required for production validation to pass, fails there.

## Declaring public model operations

`Server.AllowPublic` marks only the scopes you name:

```go
// Public sign-up; every other User operation still needs Auth coverage.
server.AllowPublic(
    maniflex.ForModel("User"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

This is a validation declaration, not middleware. It does not make a protected
route public or alter request handling.

## Other route types

Standalone files use `FilesConfig.BeforeMiddlewares`; set
`FilesConfig.AllowPublic` only when `/files` is deliberately public.

Custom actions normally inherit matching `Pipeline.Auth` middleware. If access
is enforced inside `ActionConfig.Middleware` or the handler, set
`ActionConfig.AccessControlled`. Set `ActionConfig.AllowPublic` for an
intentionally public action.

Global search likewise uses `Pipeline.Auth` for `OpSearch`, or an explicit
`GlobalSearchConfig.AllowPublic`.

If a router-level middleware protects every route before dispatch, set
`Config.HTTPAccessControlled` alongside non-empty `Config.HTTPMiddlewares`.
This flag is an assertion and does not install authentication.

Generated documentation is already explicit: its zero value mounts nothing,
`Documentation.Middleware` protects it, and `Documentation.Public` deliberately
publishes it. Static serving requires an explicit non-empty `StaticDir`.

The probe endpoints — `/live`, `/ready`, and `/health` — are public by default
and the sweep exempts them, because an orchestrator's probe is the canonical
unauthenticated request. `Config.Probes` gates or unmounts them when that is not
what you want; setting it does not change what `ValidateProduction` asks for.
