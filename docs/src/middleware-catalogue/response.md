# Response Middleware

The `maniflex/middleware/response` package shapes the outgoing response —
headers, body transforms, redactions, and observability hooks — on the
**Response** step.

## Cross-cutting headers

### `CORSHeaders`

Adds CORS headers to every response. At least one origin is **required** — pass
explicit origins (recommended) or `"*"` to allow any origin. Calling it with no
origins panics at startup, so a permissive wildcard is never applied by accident.

```go
import "github.com/xaleel/maniflex/middleware/response"

// Explicit origins (recommended)
server.Pipeline.Response.Register(response.CORSHeaders("https://app.example.com"))

// Public API: opt in to any origin explicitly
server.Pipeline.Response.Register(response.CORSHeaders("*"))
```

For credentials or custom allowed headers/methods/max-age, use
`CORSHeadersWithConfig`. `AllowCredentials` cannot be combined with a `"*"`
origin (browsers reject that combination) and panics if you try:

```go
server.Pipeline.Response.Register(response.CORSHeadersWithConfig(response.CORSConfig{
    AllowOrigins:     []string{"https://app.example.com"},
    AllowCredentials: true,
}))
```

### `AddHeader`

Sets one static header on every response:

```go
server.Pipeline.Response.Register(
    response.AddHeader("Strict-Transport-Security", "max-age=63072000"),
)
```

## Caching

### `Cache`

Sets `Cache-Control: public, max-age=N` on successful reads. Register at
`maniflex.After` so the framework's own headers do not override yours:

```go
server.Pipeline.Response.Register(
    response.Cache(300),  // 5 minutes
    maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
    maniflex.AtPosition(maniflex.After),
)
```

## Body transforms

### `TransformField`

Rewrites a single field value before serialisation. Common use: rebasing a
stored relative path onto a CDN host.

```go
server.Pipeline.Response.Register(
    response.TransformField("avatar_url", func(v any) any {
        return cdnBase + v.(string)
    }),
)
```

### `RedactField`

Hides a field from the response conditionally. The predicate decides per
request, often based on `ctx.Auth`:

```go
server.Pipeline.Response.Register(
    response.RedactField("phone", func(ctx *maniflex.ServerContext) bool {
        return !ctx.HasRole("support")
    }),
)
```

`RedactField` is the right tool for view-time access control on individual
columns. For all-or-nothing exclusion across an entire model, the `hidden` or
`writeonly` field tag is simpler.

For the write side — *"only a superuser may set this field"* — use
[`validate.FieldRole` / `validate.RestrictField`](validate.md#fieldrole--restrictfield),
which takes the same predicate on the Validate step. Note the two differ on
purpose when the predicate fails: a redacted read returns the record without the
field, while a refused write returns `403` rather than quietly dropping it.

It covers **exports too**: `GET /:model/export` masks the same field for the
same callers, and drops it from the CSV/XLSX header rather than emitting an
empty column.

#### Writing your own masking middleware

A Response-step middleware normally masks by editing `ctx.Response` after
`next()` returns. That is not enough on its own, because an export has no
`ctx.Response` — it streams its bytes during `next()` — so a middleware that
only edits one masks the JSON and leaves the export in full.

Declare the field instead, **before** calling `next()`:

```go
func maskSalary(ctx *maniflex.ServerContext, next func() error) error {
    if !ctx.HasRole("admin") {
        ctx.RedactResponseField("salary") // before next(), so the export sees it
    }
    return next()
}
```

The declaration is honoured by every read path — list, single read, create and
update echoes, and both export formats. `response.RedactField` does this for
you.

> Before v0.2.5 a masking middleware applied to JSON responses only. An app
> that hid a column from non-admins served it in full at `/:model/export`.

### `Envelope`

Replaces the default `{"data": ...}` envelope with one of your own:

```go
server.Pipeline.Response.Register(
    response.Envelope(func(ctx *maniflex.ServerContext, data any, meta *maniflex.ResponseMeta) any {
        return map[string]any{
            "result":   data,
            "paging":   meta,
            "trace_id": ctx.TraceID,
        }
    }),
)
```

Useful when integrating with a frontend or API gateway that expects a
different shape. Error responses are unaffected; only success responses are
re-enveloped.

## Observability

### `Logging`

Writes a structured access log line per request at `maniflex.After`:

```go
server.Pipeline.Response.Register(
    response.Logging(slog.Default()),
    maniflex.AtPosition(maniflex.After),
)
```

The line carries request ID, trace ID, method, path, status, duration, and the
authenticated user when set.

### `Metrics`

Records per-request metrics — count, latency, status class — into a configured
collector:

```go
server.Pipeline.Response.Register(
    response.Metrics(myCollector),
    maniflex.AtPosition(maniflex.After),
)
```

A reference Prometheus collector is provided; any sink with the same interface
works.
