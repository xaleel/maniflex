# Service Middleware

The `maniflex/middleware/service` package supplies business-logic helpers for the
**Service** step — field transforms, derived values, owner-scoping — and
side-effect helpers (events, webhooks, email) for the **DB-After** step.

## Field transforms

### `HashField`

Replaces a plaintext field with its bcrypt hash before the DB step. Standard
choice for passwords:

```go
import "github.com/xaleel/maniflex/middleware/service"

server.Pipeline.Service.Register(
    service.HashField("password"),
    maniflex.ForModel("User"),
)
```

The bcrypt cost can be configured via a second argument; the default is
suitable for production.

### `SlugifyField`

Derives a slug field from a source field on create:

```go
server.Pipeline.Service.Register(
    service.SlugifyField("title", "slug"),
    maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate),
)
```

Punctuation is stripped, spaces become hyphens, and the result is lowercased.

### `SetField`

Sets a field on every create or update based on context — typically pulling
identity from `ctx.Auth`:

```go
server.Pipeline.Service.Register(
    service.SetField("user_id", func(ctx *maniflex.ServerContext) any {
        return ctx.Auth.UserID
    }),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

### `StripField`

Removes a field from the request body (and the typed record) before the DB step.
Useful for input-only confirmation fields that should never reach the database:

```go
server.Pipeline.Service.Register(service.StripField("password_confirm"))
```

### `TimestampWhen`

Sets a timestamp column when another field transitions to a specific value —
for example, recording `published_at` the moment `status` becomes
`"published"`:

```go
server.Pipeline.Service.Register(
    service.TimestampWhen("published_at", "status", "published"),
    maniflex.ForModel("Post"),
)
```

## Authorisation

### `OwnerScope`

Forces a user-id field to the authenticated caller on create. Equivalent to
`SetField("user_id", …)` plus a refusal if the client tries to spoof a
different value:

```go
server.Pipeline.Service.Register(
    service.OwnerScope("user_id"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

## Side effects

These middleware all run on the **DB** step at `maniflex.After` position, so they
fire only when the database write has succeeded.

### `Emit`

Publishes a domain event to a configured event bus after every mutating
operation:

```go
server.Pipeline.DB.Register(
    service.Emit(myBus),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)
```

`myBus` implements the event interface from `maniflex/events`; satellite packages
exist for Kafka, NATS, RabbitMQ, and Redis. See
[Events & Background Jobs](../advanced/events-jobs.md).

### `Webhook`

POSTs the affected record to an external URL with an HMAC signature:

```go
server.Pipeline.DB.Register(
    service.Webhook(service.WebhookConfig{
        URL:    "https://hooks.example.com/orders",
        Secret: "whsec_…",
    }),
    maniflex.ForModel("Order"),
    maniflex.AtPosition(maniflex.After),
)
```

### `SendEmail`

Sends a transactional email after a write. The factory takes a mailer and a
function that builds the message from the request context:

```go
server.Pipeline.DB.Register(
    service.SendEmail(mailer, func(ctx *maniflex.ServerContext) *service.EmailMessage {
        user := ctx.DBResult.(map[string]any)
        return &service.EmailMessage{
            To:      user["email"].(string),
            Subject: "Welcome",
            Body:    "Thanks for signing up.",
        }
    }),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
    maniflex.AtPosition(maniflex.After),
)
```

## Pairing with transactions

Side-effect middleware runs *inside* the request transaction when
`maniflex.WithTransaction` is registered. If the transaction rolls back the
side-effect already happened — outbound emails do not unsend themselves. Pair
event emission with a transactional outbox (see
[Events & Background Jobs](../advanced/events-jobs.md)) when this matters.
