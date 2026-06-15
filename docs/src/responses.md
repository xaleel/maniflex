# Response Envelope

Every response from a generated route follows one of two shapes — the data
envelope or the error envelope. This page documents both, along with the
status codes the framework emits.

## Success envelope

A successful single-row response — `OpRead`, `OpCreate`, `OpUpdate`:

```json
{
  "data": {
    "id": "8c1a…",
    "title": "First post",
    "created_at": "2026-05-19T12:34:56Z",
    "updated_at": "2026-05-19T12:34:56Z"
  }
}
```

A successful list response carries the same `data` key plus a `meta` block:

```json
{
  "data": [
    { "id": "8c1a…", "title": "First post", ... },
    { "id": "9d2b…", "title": "Second post", ... }
  ],
  "meta": {
    "total": 137,
    "page": 1,
    "limit": 20,
    "pages": 7
  }
}
```

| `meta` field | Meaning |
|---|---|
| `total` | total matching rows across all pages |
| `page` | page number returned (1-based) |
| `limit` | rows per page |
| `pages` | total page count, computed as `ceil(total/limit)` |

When a request uses [cursor (keyset) pagination](querying.md#cursor-keyset-pagination)
(`?cursor=`), the `meta` block takes a different shape — no `total`/`page`/`pages`
(the count is skipped):

```json
{ "data": [ ... ], "meta": { "limit": 20, "next_cursor": "eyJ2Ijoi...", "has_more": true } }
```

`DELETE` returns `204 No Content` with no body.

## Error envelope

Every error response uses:

```json
{
  "error": {
    "code": "VALIDATION_FAILED",
    "message": "one or more fields failed validation",
    "details": [
      { "field": "email",    "message": "field \"email\" is required" },
      { "field": "password", "message": "must be at least 8 characters" }
    ]
  }
}
```

| Field | Meaning |
|---|---|
| `code` | machine-readable identifier (e.g. `NOT_FOUND`, `CONFLICT`) |
| `message` | human-readable summary |
| `details` | optional structured payload — per-field errors, raw driver detail, etc. |

The catalogue of built-in codes is in [Error Handling](errors.md).

## Status codes

| Operation | Success | Notable errors |
|---|---|---|
| `OpList` | `200 OK` | `400 INVALID_QUERY` |
| `OpRead` | `200 OK` | `404 NOT_FOUND` |
| `OpCreate` | `201 Created` | `400 INVALID_JSON`, `409 CONFLICT`, `422 VALIDATION_FAILED` |
| `OpUpdate` | `200 OK` | `404 NOT_FOUND`, `409 CONFLICT`, `422 VALIDATION_FAILED` |
| `OpDelete` | `204 No Content` | `404 NOT_FOUND` |

`HEAD` and `OPTIONS` return `200` with no body.

## Headers

Every response carries:

| Header | Source |
|---|---|
| `Content-Type: application/json` | always |
| `X-Request-Id` | echoed from chi's `RequestID` middleware |
| `X-Service-Name` | when `Config.ServiceName` is set |

Custom middleware can add more — see [Response Middleware](middleware/response.md)
for `AddHeader`, `CORSHeaders`, `Cache`, and friends.

## Computed (virtual) fields

`Server.AddComputedField` registers a derived field that appears in every
read response (create echo, single read, update echo, list rows) without
being stored:

```go
server.MustAddComputedField("Product", "stock_level",
    func(ctx context.Context, row map[string]any) (any, error) {
        return stockService.CurrentLevel(ctx, row["id"].(string))
    })
```

The function runs in the Response step after the DB row has been converted
to JSON keys, so `row`'s keys are JSON field names. For list responses each
row is processed in its own goroutine — a slow function does not serialise
a whole page.

Computed fields:

- **Cannot be filtered or sorted** — they're materialised on output only.
- **Are name-collision-checked** at registration: a name that matches a
  real model field, or that's already registered as computed, is rejected.
- **Tolerate errors per-row** — a non-nil error from the function is
  logged and the field is omitted from that row; the rest of the response
  is unaffected.
- **Run on every read path** that goes through the default Response step,
  including the create and update echoes.

Use them for derived values that change too often to denormalise (stock
level, leave balance, account balance) or that depend on external systems.

## Replacing the envelope

The default shape is good enough for most APIs, but if you integrate with a
client that expects a different layout, register `response.Envelope` from the
catalogue:

```go
import "maniflex/middleware/response"

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

Error responses are unaffected — they always use the `{"error": {…}}` shape so
clients can distinguish success from failure with a single key check.
