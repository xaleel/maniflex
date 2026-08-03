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
- Every global and per-model effective `QueryLimits` field remains positively
  bounded, as does global search's `MaxLimit` when search is mounted.
- AutoMigrate is disabled when models are registered.
- Every generated model operation has matching `Pipeline.Auth` middleware or an
  explicit public declaration.
- Standalone files, custom actions, and global search each have a protected or
  explicitly public access decision.

Framework outbound calls made through `integration.Caller` already have bounded
timeout, retry, and response-size defaults. The validator cannot inspect
arbitrary `http.Client` instances created by application code.

`TrustProxyHeaders` remains off by default, which is safe. Setting it to true is
the explicit assertion that the service is behind a trusted proxy which replaces
client-supplied forwarding headers; production validation cannot inspect network
topology.

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
