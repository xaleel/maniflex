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
is suitable for production. A cost outside bcrypt's `4..31` **panics at
construction** rather than failing per request — `Hasher(32)` used to compile and
then answer `500` to every write, and `Hasher(2)` used to become the default
silently. A `Hasher` is just `func(plaintext string) (string, error)`, so you can
supply argon2 or any other implementation.

**Tag the hashed field `mfx:"writeonly"`.** Without it the stored hash is
returned on every read, and a client that reads a record, changes one field and
submits the whole thing back sends the hash as the password — which `HashField`
hashes again. The stored value becomes a hash of a hash, the real password stops
working, and nothing in the logs says why. `HashField` warns once per model and
field when the tag is missing.

An **absent** field is skipped on update, so a caller can change other fields
without resetting the password. An **empty string** is not the same thing: it is
removed on update (a form that submits every field sends a blank password box on
an ordinary save) and refused with `400` on create, where storing `""` would
produce an account that cannot be logged into. Note `mfx:"required"` does not
cover this — it means *present*, not *non-empty*.

### The 72-byte bcrypt limit

bcrypt refuses a password longer than **72 bytes**, and that is a byte count, not
a character count. `HashField` answers `400 INVALID_FIELD` naming the limit, so
it reaches the client as the input error it is rather than as a masked `500`.

`mfx:"maxlen:72"` on the field is worth adding as a first line of defence, but it
is **not** equivalent: `maxlen` counts characters. A 25-character Japanese or
emoji passphrase is 75–100 bytes and passes that check while still being over
bcrypt's limit, which is why the refusal is implemented in the hasher rather than
left to validation. Any `Hasher` of your own can do the same by wrapping
`service.ErrClientInput` around an error the caller could have avoided.

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

Forces a user-id field to the authenticated caller on create. It is
`SetField("user_id", ctx.Auth.UserID)` — an overwrite, so any value the client
sent for that field is replaced (not rejected). A principal with an empty
`UserID` is refused with `401` rather than stamped, since every such caller would
otherwise own the record together:

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
