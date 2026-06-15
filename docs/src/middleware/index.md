# Middleware Catalogue

The catalogue is a set of ready-made middleware packages that cover the common
needs of every production API — authentication, validation, password hashing,
audit logging, caching, CORS, and so on. Each one is an ordinary
`maniflex.MiddlewareFunc` you register on the appropriate pipeline step, with the
same scoping options as any other middleware.

The packages live under `maniflex/middleware/`. Each one is its own Go module so a
project pulls in only the dependencies it actually uses:

| Package | Step | What it ships |
|---|---|---|
| [`middleware/auth`](auth.md) | Auth | JWT, API key, role gates, public-read helpers |
| [`middleware/body`](body.md) | Deserialize / Validate | body size limits, unknown-field stripping, type coercion |
| [`middleware/validate`](validate.md) | Validate | uniqueness, regex, cross-field rules, numeric precision, date ranges, conditional required |
| [`middleware/workflow`](workflow.md) | Validate | state-machine transitions with role-gated guards |
| [`middleware/service`](service.md) | Service / DB-After | password hashing, slugify, derived fields, event emission, webhooks, email |
| [`middleware/db`](db.md) | DB | tenancy, forced filters, rate limiting, audit log, cache invalidation |
| [`middleware/response`](response.md) | Response | CORS, caching, transforms, redaction, envelopes, metrics |
| [`middleware/openapi`](openapi.md) | OpenAPI.Generate | security schemes, servers, titles, custom extensions |

## How to use the catalogue

Import the package you need and register the returned middleware on the
matching pipeline step:

```go
import (
    "maniflex/middleware/auth"
    "maniflex/middleware/service"
)

server.Pipeline.Auth.Register(
    auth.JWTAuth("my-signing-secret", auth.JWTOptions{Issuer: "my-app"}),
)

server.Pipeline.Service.Register(
    service.HashField("password"),
    maniflex.ForModel("User"),
)
```

Each middleware factory returns a `maniflex.MiddlewareFunc`, so the standard
options — `ForModel`, `ForOperation`, `AtPosition`, `WithName` — apply
verbatim.

## Composition

Catalogue middleware is designed to be composable. The expected stack for a
typical REST API is roughly:

1. **Auth** — `JWTAuth` or `APIKeyAuth` populates `ctx.Auth`; `RequireRole`
   gates sensitive operations.
2. **Body** — `MaxBodySize` and `StripUnknownFields` shape input early.
3. **Validate** — built-in tag rules plus `UniqueField` and friends.
4. **Service** — `HashField`, `SetField`, `SlugifyField`, then any custom
   business logic, then `Emit` / `Webhook` / `SendEmail` on the After side.
5. **DB** — `Tenancy` or `ForceFilter` enforces row-level scoping; `AuditLog`
   and `Invalidate` run After.
6. **Response** — `CORSHeaders`, `Cache`, `RedactField`, then `Logging` /
   `Metrics` on the After side.

Mix and match freely; nothing in the catalogue is required.

## Writing your own

The catalogue is just an applied form of [Writing Middleware](../middleware.md).
If a built-in does not match your needs, write your own — the contract is the
same `func(ctx *maniflex.ServerContext, next func() error) error` signature.
