# Auth Middleware

The `maniflex/middleware/auth` package supplies authentication and authorisation
middleware. Each function returns a `maniflex.MiddlewareFunc` that either populates
`ctx.Auth` on success or aborts with `401`/`403` on failure. Most register on the
**Auth** step; the exceptions are called out where they occur — `Enforce`
(attribute policies) runs on the **DB** step so the record is available, and
`ReadAudit` runs on the **Response** step after a 2xx.

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

### Where the token is read from

By default the token comes from `Authorization`, which **must** carry the
`Bearer ` scheme — a bare token there is malformed, not merely unadorned. Point
`JWTOptions.Header` at another header to read it from there instead:

```go
auth.JWTAuth(secret, auth.JWTOptions{Header: "X-Auth-Token"})
```

A custom header accepts the token with or without the `Bearer ` prefix, so a
client that sends it out of habit is not punished for it. The scheme name is
matched case-insensitively on both, per RFC 7235 — `bearer <token>` is valid.

Tokens must carry an `exp` claim: one with no expiry is rejected
(`401 TOKEN_MISSING_EXPIRY`), since it would otherwise be valid forever. Set
`JWTOptions.AllowNoExpiry` to accept non-expiring tokens from issuers that
deliberately mint them. On the HMAC path the signing secret must be non-empty (an
empty secret panics at startup) and should be at least 32 bytes — a shorter
secret is allowed but logs a warning.

## `JWKSAuth`

Verifies asymmetric JWTs against a **rotating** JWK Set published by an identity
provider (e.g. an issuer's `/.well-known/jwks.json`), instead of pinning a single
static key. Signing keys are fetched, cached, and selected by the token header's
`kid`; an unknown `kid` triggers a rate-limited refetch, so a rotated key is
picked up without a redeploy. RSA (`RS256/384/512`) and EC (`ES256/384/512`) are
supported.

```go
server.Pipeline.Auth.Register(auth.JWKSAuth(
    "https://issuer.example.com/.well-known/jwks.json",
    auth.JWTOptions{Issuer: "https://issuer.example.com", Audience: "api"},
))
```

All `JWTOptions` (`Issuer`, `Audience`, claim mappings, `ClockSkew`) apply exactly
as with `JWTAuth` — reach for the static-key `JWTAuth` only when the key is fixed
or for offline tests. See
[Auth & Security Hardening](../advanced-topics/security.md#authentication) for the
production checklist.

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

Both failure cases return `403 FORBIDDEN`: an anonymous request (`ctx.Auth ==
nil`) and an authenticated request lacking the role are treated alike. This
differs from `RequireOwner`, which answers `401` to an anonymous caller.

## `RequireOwner`

Enforces that the authenticated user owns the record being read or written. On
create it stamps `ownerField = ctx.Auth.UserID` automatically; on read, update,
and delete it fetches the target and compares its `ownerField` to the caller —
answering **404** (not 403, so the endpoint never reveals that a record it doesn't
own exists). `ownerField` may be given as the JSON or the DB column name. Callers
holding any role in `adminRoles` bypass the check.

```go
server.Pipeline.Auth.Register(auth.RequireOwner("user_id", "admin"))
```

`RequireOwner` scopes **single-resource** operations only — a collection `GET`
still returns every row. Constrain list reads with `db.ForceFilter` or
`db.Tenancy` on the DB step.

## `Enforce` — attribute-based policies

`RequireRole` and `RequireOwner` answer *who is calling*; `Enforce` answers *may
this principal touch **this** record*, evaluating a `Policy` against the affected
row's fields. A `Policy` is a plain function:

```go
type Policy func(ctx *maniflex.ServerContext, resource map[string]any) (allow bool, err error)
```

Return `(false, nil)` for a **403 FORBIDDEN**; a non-nil error becomes a `500`.
Register on the **DB** step (not Auth), so the row is available:

```go
sameTenant := func(ctx *maniflex.ServerContext, r map[string]any) (bool, error) {
    return r["tenant_id"] == ctx.Auth.TenantID, nil
}
server.Pipeline.DB.Register(auth.Enforce(sameTenant), maniflex.ForModel("Patient"))
```

Which record the policy sees depends on the operation:

| Operation | Record checked | When |
|---|---|---|
| `OpCreate` | the proposed body (`ctx.ParsedBody`) | before the insert |
| `OpUpdate` / `OpDelete` | the current stored record | fetched before the write |
| `OpRead` | the fetched record | after the DB step |
| `OpList` | each row in turn | after the DB step; denied rows are dropped |

Compose policies with `AllOf`, `AnyOf`, and `Not`:

```go
auth.Enforce(auth.AllOf(sameTenant, auth.AnyOf(isOwner, auth.Not(isArchived))))
```

For lists the policy runs **per row after the query**, so pagination totals
reflect the pre-filter count. When the rule can be expressed as a `WHERE` clause,
prefer [`db.ForceFilter`](db.md) — it scopes in SQL, keeps totals accurate, and
never fetches rows the caller can't see.

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

The model's routes remain mounted but always return `405 METHOD_NOT_ALLOWED`.

## Token revocation and logout

A JWT is valid until its `exp` and nothing the server does can take it back.
That is fine until a user logs out, changes their password, or has their account
compromised — at which point "valid until it expires" is exactly wrong. Set
`JWTOptions.Revoker` to give the server its say back, at the cost of a lookup
per request.

```go
rev := auth.NewMemoryRevoker()

server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{Revoker: rev}))
server.Action(auth.Logout(rev, ""))                    // POST /logout
server.Action(auth.LogoutAll(rev, "", 24*time.Hour))   // POST /logout-all
```

`Logout` revokes the token the caller authenticated with — logout on this device.
`LogoutAll` revokes every token belonging to the caller, including sessions whose
`jti` the server has never seen; it is the one to call after a password change.
Both respond `204`, and both must be mounted behind the same auth middleware as
everything else (they read `ctx.Auth`).

### The two granularities

| Call | Effect |
|---|---|
| `RevokeToken(ctx, jti, exp)` | blocks one token; the entry may be dropped once `exp` passes, since the token is refused for being expired anyway |
| `RevokeUser(ctx, userID, cutoff, retainUntil)` | blocks every token for the user **issued before `cutoff`**; keep the record until `retainUntil`, which must be past the exp of the longest-lived token you mint |

The per-user cutoff is what makes "log out everywhere" possible: the outstanding
`jti` values are unknown and usually unknowable, so a per-token blocklist alone
cannot express it. Logging in again works immediately — the cutoff kills tokens
issued *before* it, not the account.

### What changes when it is on

- **A `jti` becomes mandatory.** A token without one cannot be revoked, so it is
  refused with `401 TOKEN_NOT_REVOCABLE` rather than handed a permanent
  exemption from the blocklist. Mint a unique `jti` per token.
- **An `iat` becomes required for users who have a cutoff.** A token that cannot
  be placed relative to the cutoff is refused. Tokens without `iat` keep working
  for every user who has never called `LogoutAll`.
- **A store outage refuses requests** — see below.

Error codes:

| Code | Status | Meaning |
|---|---|---|
| `TOKEN_REVOKED` | `401` | explicitly revoked; the client should discard it and log in again |
| `TOKEN_NOT_REVOCABLE` | `401` | no `jti`, so it cannot be revoked — a minting bug, not a client error |
| `REVOCATION_UNAVAILABLE` | `503` | the blocklist could not be consulted |

### Failing closed

Every `Revoker` method returns an error, and the middleware refuses the request
when a lookup fails rather than reading "I could not check" as "not revoked". A
blocklist that fails open silently un-revokes every logged-out token for the
duration of an outage — precisely when it matters most. This is why `Revoker` is
its own interface rather than a reuse of `maniflex.CacheStore`, whose `Get`
reports a miss and an outage identically.

The refusal is `503`, not `401`: the credential is fine, the server is not, and
answering `401` would push every healthy client into a re-login storm during an
incident.

### Backends

`NewMemoryRevoker()` keeps the blocklist in-process. Its limitation is
structural: a second replica does not see a logout performed by the first, and a
restart loses every entry — which un-revokes every still-unexpired token. It
suits single-replica deployments, development, and tests.

Behind more than one replica, use the Redis backend:

```go
import authredis "github.com/xaleel/maniflex/middleware/auth/redis"

rev := authredis.NewRevoker(redisClient, "myapp:revoked")
```

Or implement `auth.Revoker` yourself over any store — four methods, and the
interface is in terms of `context.Context` and `time.Time` only.

## `VerifyToken` hook

`JWTOptions.VerifyToken` is the final say on a token that has passed every other
check — signature, registered claims, and revocation. Return an error to refuse
the request with `401 INVALID_TOKEN` carrying that message.

```go
auth.JWTAuth(secret, auth.JWTOptions{
    VerifyToken: func(ctx *maniflex.ServerContext, claims map[string]any, info *maniflex.AuthInfo) error {
        if claims["tier"] != "paid" {
            return errors.New("subscription required")
        }
        info.Roles = append(info.Roles, "subscriber") // may enrich the principal
        return nil
    },
})
```

Use it for issuer-specific rules the framework does not model: a required custom
claim, a tenant allowlist, a per-user check against your own store. For
revocation specifically, prefer `Revoker` — it is the same hook point with the
blocklist and its failure semantics already written.

## `CSRF`

Protects unsafe HTTP methods (`POST`/`PUT`/`PATCH`/`DELETE`) against cross-site
request forgery. Two strategies, selected by `CSRFOptions.Mode`:

- **`CSRFDoubleSubmit`** (default) — issues a random token in a non-HttpOnly
  cookie on safe requests and requires the client to echo it in a header on
  unsafe ones.
- **`CSRFSignedToken`** — derives the expected token as `HMAC(SessionID, Secret)`;
  stateless, no cookie issued. Register it *after* the JWT step so
  `ctx.Auth.SessionID` is populated (e.g. from the token's `jti` claim).

```go
server.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{
    AllowedOrigins: []string{"https://admin.example.com", "*.example.com"},
    Secure:         true,
}))
```

Bearer-authenticated requests are **exempt by default** — a bearer token read
from JS is not an ambient credential, so it isn't CSRF-vulnerable. Set
`EnforceBearer: true` only if your bearer flow also rides browser-managed cookies.
The optional `AllowedOrigins` allowlist (exact origins or `*.host` wildcards) is
checked first on unsafe methods. Failures abort with a `403` carrying one of
`CSRF_ORIGIN_REJECTED`, `CSRF_TOKEN_MISSING`, `CSRF_COOKIE_MISSING`,
`CSRF_NO_SESSION`, or `CSRF_TOKEN_MISMATCH`.

From a login Action, hand the token to the SPA with `auth.IssueCSRFCookie(w,
opts)` (double-submit) or `auth.SignedCSRFToken(sessionID, secret)` (signed mode).

## `ReadAudit`

Writes a structured audit record after every successful read or list — the
read-side counterpart to `db.AuditLog`, for data where *who looked* is itself the
compliance control (clinical, financial). Implement a `ReadAuditSink` and register
`ReadAudit` at the `After` position on the **Response** step, so it fires only on
a 2xx:

```go
server.Pipeline.Response.Register(
    auth.ReadAudit(mySink),
    maniflex.ForModel("Patient", "LabResult"),
    maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
    maniflex.AtPosition(maniflex.After),
)
```

Each `ReadAuditRecord` carries the actor, roles, tenant, session, request/trace
IDs, client IP, and either the accessed `RecordID` (read) or the `RecordCount`
(list). Writes are **fire-and-forget** — a goroutine with a 5 s timeout, so a slow
sink never delays the response — and sink errors are discarded, so back a lossless
requirement with a durable queue.

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
