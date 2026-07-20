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
    "code": "VALIDATION_ERROR",
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
| `details` | optional structured payload — an **array** of `{field, message}` objects for per-field errors |

`details` is an array wherever it is present, including on a `409 CONFLICT` from
a unique violation. That one was a bare object until v0.3.0, so a duplicate value
answered in two shapes depending on whether the database or `validate.UniqueField`
caught it; a client that ranged over `details` had to type-switch first.

A **composite** unique constraint contributes one entry per column, so a form can
highlight every input involved:

```json
{"error": {"code": "CONFLICT", "message": "unique constraint violation",
  "details": [
    {"field": "phone_number", "message": "the combination of phone_number, owner_id is already taken"},
    {"field": "owner_id",     "message": "the combination of phone_number, owner_id is already taken"}
  ]}}
```

The message names the combination rather than a column because neither column is
in violation on its own — that phone number is fine, the pair is not.

The catalogue of built-in codes is in [Error Handling](../the-request-pipeline/errors.md).

## Status codes

| Operation | Success | Notable errors |
|---|---|---|
| `OpList` | `200 OK` | `400 INVALID_QUERY` |
| `OpRead` | `200 OK` | `404 NOT_FOUND` |
| `OpCreate` | `201 Created` | `400 INVALID_JSON`, `409 CONFLICT`, `422 VALIDATION_ERROR` |
| `OpUpdate` | `200 OK` | `404 NOT_FOUND`, `409 CONFLICT`, `422 VALIDATION_ERROR` |
| `OpDelete` | `204 No Content` | `404 NOT_FOUND` |

`HEAD` mirrors the `GET` for the same URL with the body suppressed: same status
(including `404` for a record that does not exist), same headers, same middleware
— just no body.

`OPTIONS` returns `204 No Content` with an `Allow` header listing the methods the
route accepts.

## Headers

Every response carries:

| Header | Source |
|---|---|
| `Content-Type: application/json` | always |
| `X-Request-Id` | echoed from chi's `RequestID` middleware |
| `X-Service-Name` | when `Config.ServiceName` is set |

Custom middleware can add more — see [Response Middleware](../middleware-catalogue/response.md)
for `AddHeader`, `CORSHeaders`, `Cache`, and friends.

## Computed (virtual) fields

`Server.AddComputedField` registers a derived field that appears in every
read response (create echo, single read, update echo, list rows) without
being stored:

```go
server.MustAddComputedField("Product", "stock_level",
    func(ctx *maniflex.ServerContext, row map[string]any) (any, error) {
        return stockService.CurrentLevel(ctx.Ctx, row["id"].(string))
    })
```

The function runs in the Response step after the DB row has been converted
to JSON keys, so `row`'s keys are JSON field names. It receives the
`*ServerContext`, so it can reach `ctx.Tx`, `ctx.GetModel` and `ctx.Auth`.

Computed fields:

- **Cannot be filtered or sorted** — they're materialised on output only.
- **Are name-collision-checked** at registration: a name that matches a
  real model field, or that's already registered as computed, is rejected.
- **Tolerate errors per-row** — a non-nil error from the function is
  logged and the field is omitted from that row; the rest of the response
  is unaffected.
- **Tolerate a panic the same way** — a panicking callback is logged
  (with its stack, at `ERROR`) and its field omitted, exactly as a
  returned error is. It costs that one field on that one row: the record
  is still returned and the model's other computed fields still resolve.
  This holds for batch callbacks too, so converting a per-row field to a
  batch one does not change what a bad row costs.
- **Run on every read path** that goes through the default Response step,
  including the create and update echoes.
- **Must be goroutine-safe.** On a multi-row page the per-row callbacks run
  concurrently (bounded to 8 at a time), so a callback that writes to a
  captured variable, a shared map, or any other state must synchronise it.
  Batch callbacks are called once for the whole page and are not affected.

> **Do not use `ctx.Tx` from a per-row callback.** A `*sql.Tx` is not safe
> for concurrent use, and the callbacks for one page share the same
> `ServerContext`. Reads through `ctx.GetModel` or `maniflex.Read` enlist
> `ctx.Tx` when one is open, so a computed field that reads while the
> request is in a transaction is exactly this case. Resolve such a field
> with `AddBatchComputedField`, which runs once and sequentially.
- **Appear in the OpenAPI spec** as read-only properties of the model's
  response schema (never in a create or update body).

Use them for derived values that change too often to denormalise (stock
level, leave balance, account balance) or that depend on external systems.

### Batch resolution (`AddBatchComputedField`)

`AddComputedField` runs **once per row**, so a resolver that queries is an
N+1: a 50-row page costs 50 round-trips. `AddBatchComputedField` resolves
the whole page in one call instead — this is what lets a generated
`GET /store-sites` return an `item_count` without a hand-written action:

```go
server.MustAddBatchComputedField("StoreSite", "item_count",
    func(ctx *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
        ids := make([]any, len(rows))
        for i, r := range rows {
            ids[i] = r["id"]
        }
        counts, err := itemCountsBySite(ctx, ids) // ONE query for the page
        if err != nil {
            return nil, err
        }
        out := make([]any, len(rows))
        for i, r := range rows {
            out[i] = counts[r["id"].(string)]
        }
        return out, nil
    },
    maniflex.ComputedSchema(&maniflex.OASSchema{Type: "integer"}))
```

The callback must return **exactly one value per row, positionally aligned
to `rows`**. A length mismatch is logged and the field is omitted from the
whole response rather than landed on the wrong records — an absent column is
diagnosable, a misaligned one is not.

One registration serves every read path: a single read and the create/update
echo call it with a one-row slice, and an export calls it once per chunk of
rows (so a batch field costs one call per 500 records there, not one per
record).

**Prefer the batch form for anything that touches a database.** Per-row
resolvers run concurrently across a page, but bounded at 8 at a time — the
fan-out used to be one goroutine per row with no ceiling, so a 100-row page
fired 100 concurrent round-trips and the load scaled as page-size ×
concurrent-requests. The bound stops that from being unbounded; it does not
stop it from being an N+1. Note too that work through `ctx.Tx` is serialised
by the transaction's single connection, so per-row parallelism buys nothing
there.

### Declaring the type

Both callbacks return `any`, so the framework cannot infer a computed
field's type. Without `ComputedSchema` the field still appears in the spec
(read-only) but carries no type — a generated client knows it exists but not
what it holds. `maniflex.ComputedSchema(&maniflex.OASSchema{…})` declares it.

### Typed variants

`maniflex.AddComputedField[T]` and `maniflex.AddBatchComputedField[T]` take
the loaded record(s) as `*T` / `[]*T` instead of JSON maps:

```go
maniflex.AddBatchComputedField(server, "StoreSite", "item_count",
    func(ctx *maniflex.ServerContext, sites []*StoreSite) ([]any, error) {
        // …one query, one value per site
    })
```

## Replacing the envelope

The default shape is good enough for most APIs, but if you integrate with a
client that expects a different layout, register `response.Envelope` from the
catalogue:

```go
import "github.com/xaleel/maniflex/middleware/response"

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
