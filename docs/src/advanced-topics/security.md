# Auth & Security Hardening

The defaults are safe to deploy, but production APIs benefit from a few extra
layers. This page collects the practical checklist.

## Authentication

- **Use `auth.JWTAuth` with an asymmetric algorithm** (`RS256` / `ES256`) when
  tokens are issued by an external provider. Symmetric `HS256` works when the
  signing service and the API share infrastructure.
- **Use `auth.JWKSAuth(jwksURL, opts…)` when the issuer publishes a rotating
  JWK Set** (`/.well-known/jwks.json`). It fetches and caches the keys, selects
  the signing key by the token's `kid`, and refetches on an unknown `kid` so key
  rotation needs no redeploy. All `JWTOptions` (Issuer, Audience, claim mappings,
  ClockSkew) apply. Prefer this over pinning a single static `PublicKey` against
  an issuer that rotates. RSA (`RS256/384/512`) and EC (`ES256/384/512`) supported.
- **Set `JWTOptions.Issuer` and `Audience`** so tokens issued for another
  audience are rejected.
- **Set `JWTOptions.TenantClaim`** for multi-tenant APIs — the verified value
  ends up on `ctx.Auth.TenantID` and feeds `db.Tenancy`.
- **Never accept anonymous writes by default.** Register `auth.JWTAuth` (or
  `auth.APIKeyAuth`) on the Auth step scoped to `OpCreate`, `OpUpdate`,
  `OpDelete`. Use `auth.AllowPublicRead` when reads are truly public.

```go
server.Pipeline.Auth.Register(auth.AllowPublicRead())
server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{
    Issuer:      "https://accounts.example.com",
    Audience:    "https://api.example.com",
    TenantClaim: "org_id",
}), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
```

Verifying tokens from an issuer that rotates its signing keys via JWKS:

```go
server.Pipeline.Auth.Register(auth.JWKSAuth(
    "https://accounts.example.com/.well-known/jwks.json",
    auth.JWTOptions{
        Issuer:      "https://accounts.example.com",
        Audience:    "https://api.example.com",
        TenantClaim: "org_id",
    }))
```

## Authorisation

- **Gate sensitive operations with `auth.RequireRole`**. Don't rely on the UI
  to hide them.
- **Use `db.Tenancy` or `db.ForceFilter`** for row-level scoping. These run on
  the DB step so they apply to lists, reads, and writes uniformly — UI code
  cannot accidentally bypass them.
- **Strip privileged values with `validate.ForbiddenValues`** for role fields
  and similar — prevent a normal user from promoting themselves by including
  `"role": "admin"` in a payload.

## Secrets and PII

- **Hash passwords with `service.HashField`**, never store them raw.
- **Use `writeonly` on credential fields** so they are accepted on input but
  never returned in responses.
- **Encrypt sensitive columns** with `mfx:"encrypted"` and a configured
  `KeyProvider`. Pair with the `key:` sub-option for per-domain keys.
- **Redact in responses with `response.RedactField`** when a column is
  visible to some callers and hidden from others.

## Input

- **Set `Config.QueryTimeout`** so a slow query can't tie up a connection
  indefinitely.
- **Cap body sizes with `body.MaxBodySize`** where you know the upper bound.
  The default 4 MB limit catches accidents, but a 10 KB endpoint should
  enforce 10 KB.
- **Strip unknown fields with `body.StripUnknownFields`** in environments
  where you want a strict contract — every accepted field appears on the
  model.
- **Validate beyond tags** with `validate.RegexField`, `validate.UniqueField`,
  and `validate.CrossFieldValidate`. The built-in `mfx:` rules cover the
  common cases; everything else belongs in middleware.

## Output

- **Set security headers globally** via `response.AddHeader`:
  `Strict-Transport-Security`, `X-Content-Type-Options`, `Referrer-Policy`.
- **List CORS origins explicitly** with `response.CORSHeaders(origins...)` —
  origins are required (there is no permissive wildcard default; it panics if you
  pass none), and `"*"` cannot be combined with credentials.
- **Cap rate-sensitive endpoints** with `db.RateLimit` so password resets and
  similar can't be brute-forced.

## Transport

- **Terminate TLS at the load balancer or reverse proxy**, not in the maniflex
  process. The framework is HTTP/1.1 + HTTP/2 ready.
- **Set `Config.TrustProxyHeaders: true` only when behind a trusted proxy.**
  It is **off by default**: the client IP is the direct TCP peer, so a caller
  cannot forge it. When on, the IP is read from `X-Forwarded-For` / `X-Real-IP`,
  which is only safe if the proxy overwrites (not appends) any inbound value the
  client sent. Every IP-keyed feature — `db.RateLimit`, idempotency scoping, and
  read-audit records — depends on this being correct; leaving it off while
  directly internet-facing keeps per-IP limits and audit logs honest.
- **Set `Config.PathPrefix` to a non-default value** if the proxy mounts the
  API at a custom path. Don't rewrite paths inside the application.

## Operations

- **Use a JSON-emitting `slog` handler** in production so logs are structured
  and ingestable by your aggregator.
- **Set `Config.ServiceName`** — every log line and audit record carries it.
- **Enable `Config.HealthCheckDB`** for Kubernetes readiness probes; tune
  `Config.HealthTimeout` shorter than the probe timeout.
- **Use `Config.PanicLogger`** to route panics to a different sink than the
  rest of the framework logs, so they are easier to alert on.

## Audit

- **Register `db.AuditLog`** at `maniflex.After` for mutating operations. The
  records carry actor, model, operation, and a diff of the affected row.
- **Use `maniflex.ModelConfig{Versioned: true}`** on sensitive models. Every
  change writes a row to a sibling `{model}_history` table.

## Checklist

A reasonable production stack:

```go
// Auth
server.Pipeline.Auth.Register(auth.JWTAuth(secret, jwtOpts))
server.Pipeline.Auth.Register(auth.RequireRole("admin"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpDelete))

// Body
server.Pipeline.Deserialize.Register(body.MaxBodySize(32<<10),
    maniflex.ForModel("PasswordReset"))
server.Pipeline.Validate.Register(body.StripUnknownFields())

// DB
server.Pipeline.DB.Register(db.Tenancy("org_id", tenantFromAuth))
server.Pipeline.DB.Register(db.RateLimit(db.RateLimitConfig{
    RequestsPerMinute: 10,
    Key:               keyByIP,
}), maniflex.ForModel("PasswordReset"))
server.Pipeline.DB.Register(db.AuditLog(auditSink),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After))

// Response
server.Pipeline.Response.Register(response.CORSHeaders("https://app.example.com"))
server.Pipeline.Response.Register(
    response.AddHeader("Strict-Transport-Security", "max-age=63072000"))
server.Pipeline.Response.Register(response.Logging(slog.Default()),
    maniflex.AtPosition(maniflex.After))
```
