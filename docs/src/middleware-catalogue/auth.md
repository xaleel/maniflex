# Auth Middleware

The `maniflex/middleware/auth` package supplies authentication and authorisation
middleware for the **Auth** pipeline step. Each function returns a
`maniflex.MiddlewareFunc` that populates `ctx.Auth` on success or aborts with `401`
or `403` on failure.

## `JWTAuth`

Verifies a bearer JWT and populates `ctx.Auth` from its claims.

```go
import "github.com/xaleel/maniflex/middleware/auth"

server.Pipeline.Auth.Register(
    auth.JWTAuth("my-signing-secret", auth.JWTOptions{
        Issuer:       "my-app",
        Audience:     "api",
        TenantClaim:  "org_id",   // copied into AuthInfo.TenantID
        ScopesClaim:  "scope",    // copied into AuthInfo.Scopes
    }),
)
```

Supports HMAC (`HS256/384/512`) when the secret is a string and asymmetric
algorithms (`RS256/384/512`) when `JWTOptions.PublicKey` is set — useful with
external identity providers (Auth0, Okta, Cognito, etc.). `AuthMethod` on
`ctx.Auth` is set to `"jwt"`.

Tokens must carry an `exp` claim: one with no expiry is rejected
(`401 TOKEN_MISSING_EXPIRY`), since it would otherwise be valid forever. Set
`JWTOptions.AllowNoExpiry` to accept non-expiring tokens from issuers that
deliberately mint them. On the HMAC path the signing secret must be non-empty (an
empty secret panics at startup) and should be at least 32 bytes — a shorter
secret is allowed but logs a warning.

## `APIKeyAuth`

Validates a static API key from a request header. Each entry maps one key to
the `AuthInfo` it grants.

```go
server.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
    auth.APIKeyEntry{Key: "svc-abc", Auth: maniflex.AuthInfo{
        UserID: "svc-1", Roles: []string{"admin"},
    }},
    auth.APIKeyEntry{Key: "svc-xyz", Auth: maniflex.AuthInfo{
        UserID: "svc-2", Roles: []string{"reader"},
    }},
))
```

`AuthMethod` on `ctx.Auth` is set to `"api_key"`. Combine with `JWTAuth` on
the same step to accept either credential — the first match wins.

## `RequireRole`

Rejects the request unless `ctx.Auth.Roles` contains the named role. Typically
registered with `ForModel` / `ForOperation` to scope where the check applies.

```go
server.Pipeline.Auth.Register(
    auth.RequireRole("admin"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpDelete),
)
```

Anonymous requests (`ctx.Auth == nil`) are rejected with `401`; authenticated
requests without the role get `403`.

## `AllowPublicRead`

A passthrough that exempts read operations from upstream auth requirements.
Register it *before* `JWTAuth` / `APIKeyAuth` to let unauthenticated callers
hit `GET` routes while keeping writes locked down.

```go
server.Pipeline.Auth.Register(auth.AllowPublicRead())
server.Pipeline.Auth.Register(auth.JWTAuth("..."),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

## `BlockOperation`

Refuses the listed operations outright, regardless of identity. Useful for
making a model effectively read-only at the HTTP layer.

```go
server.Pipeline.Auth.Register(
    auth.BlockOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.ForModel("AuditLog"),
)
```

The model's routes remain mounted but always return `405 METHOD_BLOCKED`.

## Scoping patterns

The middleware in this package combines with `ForModel` / `ForOperation` to
build per-route policy without writing custom Auth code:

```go
// Public reads, JWT writes, admin-only deletes
server.Pipeline.Auth.Register(auth.AllowPublicRead())
server.Pipeline.Auth.Register(auth.JWTAuth("..."),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
server.Pipeline.Auth.Register(auth.RequireRole("admin"),
    maniflex.ForOperation(maniflex.OpDelete))
```
