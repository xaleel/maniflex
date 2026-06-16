# Body Middleware

The `maniflex/middleware/body` package shapes the request body during the
**Deserialize** and **Validate** steps — before the framework's tag rules run.

## `MaxBodySize`

Overrides the default 4 MB body limit for the current request. Register on
the Deserialize step, scoped to the model that needs the larger limit:

```go
import "github.com/xaleel/maniflex/middleware/body"

server.Pipeline.Deserialize.Register(
    body.MaxBodySize(16 << 20),  // 16 MB
    maniflex.ForModel("Article"),
)
```

Requests over the limit are aborted with `400 BODY_READ_ERROR` before the JSON
parser runs.

## `StripUnknownFields`

Removes keys from `ctx.ParsedBody` that do not correspond to a model field.
Register on the Validate step (or Deserialize After-position) so the cleanup
happens before tag validation and the DB step:

```go
server.Pipeline.Validate.Register(body.StripUnknownFields())
```

The default behaviour is to accept and silently ignore unknown fields. Use
this middleware to enforce a stricter contract when desired.

## `CoerceTypes`

Coerces string values in `ctx.ParsedBody` into the Go type declared on the
model — `"42"` → `42`, `"true"` → `true`, ISO-8601 strings → `time.Time`. Helps
when the client sends form-encoded or query-string-shaped payloads.

```go
server.Pipeline.Validate.Register(body.CoerceTypes())
```

Coercion happens before the framework's `min` / `max` / `enum` checks, so
numeric ranges and enums work against the coerced values.
