# Service Middleware

The `maniflex/middleware/service` package supplies business-logic helpers for the
**Service** step — field transforms, derived values, and owner-scoping.
Side-effect helpers (events, webhooks, email) live in the separate
[`maniflex/events`](../advanced-topics/events-jobs.md) package, not here — see
[Side effects](#side-effects) below.

## Field transforms

### `HashField`

Replaces a plaintext field with its hash before the DB step, using a `Hasher`
you supply. The bcrypt hasher lives in the satellite package
`maniflex/middleware/service/bcrypt` (kept separate so the core has no bcrypt
dependency):

```go
import (
    "github.com/xaleel/maniflex/middleware/service"
    svcbcrypt "github.com/xaleel/maniflex/middleware/service/bcrypt"
)

server.Pipeline.Service.Register(
    service.HashField("password", svcbcrypt.Hasher()),
    maniflex.ForModel("User"),
)
```

`svcbcrypt.Hasher()` takes an optional cost (`svcbcrypt.Hasher(12)`); the default
is suitable for production. A `Hasher` is just `func(plaintext string) (string,
error)`, so you can supply argon2 or any other implementation.

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

### `Timestamp`

Unconditionally sets a timestamp column to the current time on every write it
runs for — use `ForOperation` to scope it (e.g. a `last_seen_at` touched on
update):

```go
server.Pipeline.Service.Register(
    service.Timestamp("last_seen_at"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpUpdate),
)
```

### `CopyField`

Copies one field's value into another before the DB step — for denormalising a
value the client shouldn't set directly:

```go
server.Pipeline.Service.Register(
    service.CopyField("email", "billing_email"),
    maniflex.ForModel("Account"), maniflex.ForOperation(maniflex.OpCreate),
)
```

## Authorisation

### `OwnerScope`

Forces a user-id field to the authenticated caller on create. It is exactly
`SetField("user_id", ctx.Auth.UserID)` — an unconditional overwrite, so any value
the client sent for that field is replaced (not rejected):

```go
server.Pipeline.Service.Register(
    service.OwnerScope("user_id"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

## Side effects

Side effects — events, webhooks, email — are **not** in this package. They live
in [`maniflex/events`](../advanced-topics/events-jobs.md), and only `events.Emit`
is pipeline middleware; `events.Webhook` and `events.SendEmail` are event-bus
*subscribers*, not middleware, so they are wired with `bus.Subscribe`, not
`Pipeline.Register`.

`events.Emit` publishes a domain event on the **DB** step at `maniflex.After`,
after the write succeeds:

```go
import "github.com/xaleel/maniflex/events"

server.Pipeline.DB.Register(
    events.Emit(bus),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)
```

Webhooks and email then react to those events as subscribers:

```go
bus.Subscribe(ctx, events.Subscription{
    Patterns: []string{"order.*"},
    Handler:  events.Webhook(events.WebhookConfig{URL: "https://hooks.example.com/orders", Secret: "whsec_…"}),
})
```

`bus` is any `events.Bus` — `events/inproc` for a single binary, or the Kafka,
NATS, RabbitMQ, and Redis adapters. See
[Events & Background Jobs](../advanced-topics/events-jobs.md) for the full API,
including the transactional outbox that makes `events.Emit` commit atomically
with the write (without it, a rolled-back transaction leaves an already-sent
side effect — outbound emails do not unsend themselves).
