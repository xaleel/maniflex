# Auth & Security Hardening

The defaults are safe to deploy, but production APIs benefit from a few extra
layers. This page collects the practical checklist.

## Authentication

- **Use `auth.JWTAuth` with an asymmetric algorithm** (`RS256` / `ES256`) when
  tokens are issued by an external provider. Symmetric `HS256` works when the
  signing service and the API share infrastructure.
- **Publish the JWK Set over `https://`.** It is the only thing deciding which
  tokens verify, so plaintext hands anyone on the path the ability to mint their
  own; a non-loopback `http://` URL warns at startup. See
  [`JWKSAuth`](../middleware-catalogue/auth.md#the-jwks-url-must-be-https).
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
  `auth.APIKeyAuth`) on the Auth step *unscoped*, and open the genuinely public
  routes with `auth.AllowAnonymous`. Scoping the authenticator onto a list of
  operations or models instead leaves everything it does not name covered by no
  auth registration at all — and `ForModel`/`ForOperation` are inclusion-only, so
  the next model added is on the wrong side of that line with nothing to say so.

```go
server.Pipeline.Auth.Register(auth.AllowAnonymous(),
    maniflex.ForOperation(maniflex.OpList, maniflex.OpRead))
server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{
    Issuer:      "https://accounts.example.com",
    Audience:    "https://api.example.com",
    TenantClaim: "org_id",
}))
```

`AllowAnonymous` forgives an **absent** credential only. One that was presented
and failed — expired, wrongly signed, revoked, malformed — is still `401` on an
exempt route, so a caller cannot shed a restrictive role by corrupting a byte of
their own token.

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
- **Gate writes with `validate.FieldRole`** when a column is writable by some
  callers and not others (`readonly` is all-or-nothing). Without it, a
  privileged field needs its own endpoint to keep it off the general PATCH.

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
  pass none), and `"*"` cannot be combined with credentials. Install it in
  `Config.HTTPMiddlewares`, where preflight runs before Auth.
- **Cap rate-sensitive endpoints** with `db.RateLimit` so password resets and
  similar can't be brute-forced.

## Transport

- **Terminate TLS at the load balancer or reverse proxy**, not in the maniflex
  process. The framework is HTTP/1.1 + HTTP/2 ready.
- **Name your proxies in `Config.TrustedProxies`, not just `TrustProxyHeaders`.**
  Proxy-header resolution is **off by default**: the client IP is the direct TCP
  peer, so a caller cannot forge it. Every IP-keyed feature — `db.RateLimit`,
  idempotency scoping, and read-audit records — depends on that address, so how
  you turn resolution on matters:

  ```go
  Config{TrustedProxies: []string{"10.0.0.0/8"}} // your LB's CIDRs
  ```

  Headers are then believed only from those peers, and the `X-Forwarded-For`
  chain is walked right-to-left past them — so the first address no trusted proxy
  vouched for wins. A client connecting directly cannot forge its address at all,
  and one behind the proxy cannot forge it either: a proxy *appends* the address
  it saw, so anything the client wrote sits to the left of the truth and is
  skipped. That holds whether the proxy extends the client's header line (nginx,
  AWS ALB) or adds one of its own below it (HAProxy's `option forwardfor`) — every
  line is joined into one chain before the walk. A non-empty list enables
  resolution on its own; `TrustProxyHeaders` is not also required.

  `TrustProxyHeaders: true` **without** a list is the legacy mode: the leftmost
  `X-Forwarded-For` entry, from any peer — which is the entry a client controls.
  It is safe only if the proxy strips both inbound headers itself. It warns at
  startup and fails under `Config.Strict`.
- **Set `Config.PathPrefix` to a non-default value** if the proxy mounts the
  API at a custom path. Don't rewrite paths inside the application.
- **Register `auth.CSRF` if — and only if — browsers authenticate with
  cookies.** A bearer token read from JavaScript is not an ambient credential, so
  a token-authenticated API is not CSRF-vulnerable and the middleware exempts
  bearer requests by default. Cookie-borne sessions are the case that needs it.
  See [CSRF](../middleware-catalogue/auth.md#csrf) for both modes. The admin
  panel carries its own, unconditionally — see
  [Admin Panel](../deployment/admin.md#csrf-protection) — and configuring one
  does not affect the other.

## Operations

- **Use a JSON-emitting `slog` handler** in production so logs are structured
  and ingestable by your aggregator.
- **Set `Config.ServiceName`** — every log line and audit record carries it.
- **Point `readinessProbe` at `{prefix}/ready` and `livenessProbe` at
  `{prefix}/live`**; tune `Config.HealthTimeout` shorter than the probe timeout.
  A liveness probe aimed at a database-backed endpoint turns a dependency
  outage into a restart loop across every replica.
- **Leave `Probes.PublishReadinessChecks` off** unless `{prefix}/ready` is
  reachable only from inside the cluster. It writes the *names* of your
  dependencies and which are failing into a body the probes serve without
  authentication — they bypass `Pipeline.Auth` by design. `Config.Probes` also
  gates or unmounts each probe; see
  [Gating and unmounting the probes](../deployment/config.md#gating-and-unmounting-the-probes).
  Gate `/ready`, not `/live`: a 401 from a liveness probe gets the container
  killed mid-drain.
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
// HTTP/router layer — configure before maniflex.New
cfg.HTTPMiddlewares = append(cfg.HTTPMiddlewares,
    response.CORSHeaders("https://app.example.com"))
server := maniflex.New(cfg)

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

// Response pipeline
server.Pipeline.Response.Register(
    response.AddHeader("Strict-Transport-Security", "max-age=63072000"))
server.ObserveRequests(response.Logging(slog.Default()))
```
