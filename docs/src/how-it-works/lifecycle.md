# Request Lifecycle

This page traces a single request through every piece of the framework. The
example is a `POST /api/orders` on an authenticated user, with a Service
middleware that hashes a derived field, a transaction wrapping the DB step,
and an audit-log middleware on DB-After. It exercises the full pipeline
without being contrived.

## Setup

```go
server.MustRegister(models.Order{})

server.Pipeline.Auth.Register(auth.JWTAuth("secret"))
server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
server.Pipeline.Service.Register(
    service.SetField("customer_id", func(ctx *maniflex.ServerContext) any {
        return ctx.Auth.UserID
    }),
    maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate),
)
server.Pipeline.DB.Register(
    db.AuditLog(sink, db.WithChanges()),
    maniflex.ForModel("Order"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
server.Pipeline.Response.Register(response.CORSHeaders("https://app.example.com"))
```

## The request

```
POST /api/orders HTTP/1.1
Authorization: Bearer eyJ...
Content-Type: application/json
Idempotency-Key: 2af9...

{"total": 42.50, "status": "pending"}
```

## What happens, in order

### 1. The router selects the route

chi matches `POST /api/orders` to the sub-router mounted by `mountModel`
for the `Order` model. The matched handler calls `handler.Create(meta)`,
which:

- Allocates a fresh `*ServerContext`.
- Sets `ctx.Request`, `ctx.Writer`, `ctx.Ctx`.
- Reads the `X-Request-Id` chi added in the outer middleware and stores it
  on `ctx.RequestID`.
- Reads the `traceparent` header (if present) into `ctx.TraceID`.
- Sets `ctx.Model = meta` for `Order`.
- Sets `ctx.Operation = OpCreate`.
- Leaves `ctx.ResourceID` empty — create has no path id.
- Calls `Pipeline.execute(ctx)`.

If `Config.QueryTimeout` is non-zero, `ctx.Ctx` is wrapped in a
`context.WithTimeout` here, so every downstream DB call inherits the
deadline.

### 2. The Auth step runs

`Pipeline.Auth.build("Order", OpCreate)` returns a chain consisting of
matching middleware in registration order, then the default Auth handler at
the end (which is a passthrough).

For our setup the chain is just `[auth.JWTAuth, defaultAuth]`. `JWTAuth`:

- Reads the `Authorization` header.
- Verifies the signature with the configured secret.
- Parses the claims and populates `ctx.Auth = &AuthInfo{UserID: …, Roles: …, TenantID: …}`.
- Calls `next()` to continue.

A missing or invalid token would have produced `ctx.Abort(401, "UNAUTHORIZED", …)`
and returned without `next()`, ending the request right here.

### 3. The Deserialize step runs

The default handler parses two things:

- **Query parameters** → `ctx.Query` (a `*QueryParams`). For a create, this
  is mostly empty — there are no `filter` or `sort` to read.
- **Body**. The `Content-Type` is `application/json`, so it reads up to 4 MB
  from `ctx.Request.Body`, sets `ctx.RawBody` to the raw bytes, and parses the
  JSON into the read-only `ctx.ParsedBody` (a `*RequestBody`):

```go
// ctx.ParsedBody now holds { "total": 42.5, "status": "pending" }
total, _ := ctx.Field("total")   // 42.5
status, _ := ctx.Field("status") // "pending"
```

  The same values are bound to the typed record `ctx.Record`; middleware mutate
  either through `ctx.SetField` / `ctx.DeleteField`.

If the `Content-Type` had been `multipart/form-data`, the default handler
would route through `parseMultipart` instead, populating `ctx.ParsedBody`
with form fields and `ctx.Files` with the file parts.

After-position middleware on Deserialize (e.g. `idempotency.Middleware`,
which sees `ctx.RawBody`) runs next. With idempotency configured, the
middleware would compute a body hash and either replay a cached response or
fall through to step 4.

### 4. The Validate step runs

The default handler iterates `ctx.Model.Fields` and applies the `mfx:` tag
rules to `ctx.ParsedBody`:

- `id` is stripped — the adapter assigns it.
- `readonly` fields (`created_at`, `updated_at`) are stripped.
- `immutable` fields are stripped *if `OpUpdate`* — not on create.
- `required` fields must be present. `total` and `status` are required; the
  request supplies both, so no error.
- `enum` membership is checked on `status` — `"pending"` is in the allowed
  set, so no error.
- `min` / `max` are checked on numeric fields when present.

If any rule had failed, the step would have called `ctx.Abort(422, "VALIDATION_FAILED", …)`
with `details: [...]` listing the bad fields.

Custom Validate middleware (none in this example) would run alongside the
default — `Before` middleware first, then the default, then `After`.

### 5. The Service step runs

Two middleware are scoped to this request:

**`maniflex.WithTransaction(nil)`** runs first:

- Sees `ctx.Tx == nil`.
- Calls `ctx.BeginTx(ctx.Ctx, nil)`, which delegates to the adapter.
- Assigns the resulting `Tx` to `ctx.Tx` and re-wraps `ctx.Ctx` with the tx
  stored under `txContextKey{}`.
- Defers `tx.Rollback()` — a no-op after `Commit`.
- Calls `next()` to run the rest of the pipeline inside the transaction.

**`service.SetField("customer_id", ...)`** runs second:

- Resolves the callback against `ctx.Auth.UserID`.
- Calls `ctx.SetField("customer_id", "user-alice")`, writing through to both
  `ctx.ParsedBody` and the typed `ctx.Record`.
- Calls `next()`.

### 6. The DB step runs

The default DB step calls `defaultSteps.db`:

- Sees `ctx.Tx != nil` and constructs a `dbExec{adapter, tx: ctx.Tx}`.
- Builds the DB-column write set from the typed `ctx.Record` (falling back to
  `toDBMap(ctx.ParsedBody)` for bodies the record can't represent); here the
  column names match the JSON keys.
- If the model had `mfx:"encrypted"` fields, calls `encryptFields` to
  replace plaintexts with `enc:<base64>` envelopes and write `{field}_hmac`
  companions for unique ones.
- Dispatches by `ctx.Operation`:

```go
result, err := exec.Create(ctx.Ctx, model, dbData)
```

`exec.Create` on a transactional `dbExec` calls `tx.Create(...)`, which
runs `INSERT INTO orders (...) RETURNING *` (Postgres) or
`INSERT ... ; SELECT ...` (SQLite).

The adapter returns the inserted row as a `map[string]any`. The DB step
assigns it to `ctx.DBResult`.

If the adapter returned `maniflex.ErrNotFound`, the step would abort with
`404 NOT_FOUND`. `*maniflex.ErrConstraint` becomes `409 CONFLICT`. A
context-cancelled error becomes `504 TIMEOUT`. Any other adapter error
becomes `500 DATABASE_ERROR`.

#### 6a. Audit-log Before middleware

We registered `db.AuditLog` at the default `Before` position (because
`WithChanges()` needs to read the pre-image). On `OpCreate` there is no
pre-image, so the middleware merely sets up to collect the post-image. It
calls `next()`, which runs the rest of the chain — the default DB handler
above.

After `next()` returns and `ctx.Response` is still nil (the create
succeeded), the middleware:

- Reads `ctx.DBResult` for the inserted row.
- Builds an `AuditRecord` with model, operation, actor (`ctx.Auth.UserID`),
  tenant, request id, trace id, and a diff of every changed field.
- Spawns a goroutine that calls `sink.Write(bgCtx, record)`. Audit writes
  are fire-and-forget — a sink error never fails the request.

### 7. `WithTransaction` commits

Control returns to `WithTransaction` (because we are inside its `next()`
call). It checks:

- `next()` returned nil → no pipeline error.
- `ctx.Response == nil` or `< 400` → no aborted step.
- Calls `tx.Commit()`. The deferred `Rollback` is now a no-op.
- Clears `ctx.Tx` so any post-commit code uses the bare adapter.

If `next()` had returned an error, or if any step had set `ctx.Response`
to a status `>= 400`, `Commit` would have been skipped and the deferred
`Rollback` would have fired.

### 8. The Response step runs

The default Response handler:

- Sees `ctx.Response == nil`.
- Sees `ctx.Operation == OpCreate`.
- Builds:

```go
ctx.Response = &APIResponse{
    StatusCode: http.StatusCreated,
    Data:       toJSONMap(ctx.DBResult.(map[string]any), model),
}
```

`toJSONMap` converts DB column names back to JSON field names and applies
`hidden` and `writeonly` filtering — any column tagged those is dropped
from the response shape.

After-position middleware on Response runs next. `response.CORSHeaders`
adds the appropriate `Access-Control-*` headers via `ctx.Writer.Header()`.

### 9. The envelope is written to the wire

`APIResponse.Write(ctx.Writer)`:

- Sets `Content-Type: application/json`.
- Writes the status code header (`201 Created`).
- Encodes `{"data": {...}}` to the response body.
- Returns.

chi's RequestID middleware (registered at the router root, outside the maniflex
pipeline) wraps the whole exchange — it has already set `X-Request-Id` on the
response by the time we get here. chi's RealIP is registered alongside it only
when `Config.TrustProxyHeaders` is set, so `RemoteAddr` reflects the forwarded
client IP just for servers that opted into trusting proxy headers.

## The dispatch cleanup

After the response is written, the handler runs its cleanup phase:

- Closes any open multipart file readers in `ctx.Files`.
- Removes the multipart temporary directory.
- Lets the `*ServerContext` go out of scope; it is garbage-collected with the
  request.

The framework does not pool or reuse `ServerContext` values. The per-request
allocation is small; the simplicity is worth more than the saved
allocations.

## What changes for other operations

Different operations exercise slightly different paths:

- **`OpRead` / `OpList`** — Validate runs but is a no-op (it only fires
  for create/update). The DB step calls `FindByID` or `FindMany`. Response
  for List wraps with `meta: {total, page, limit, pages}`.
- **`OpUpdate`** — Like create, but `ctx.ResourceID` is set, `immutable`
  fields are stripped in Validate, and the DB step calls `Update`. The
  audit middleware fetches the pre-image before `next()` so the `Changes`
  diff has both sides.
- **`OpDelete`** — No body to deserialize, no Validate work. The DB step
  calls `Delete` (which becomes a soft-delete `UPDATE` for models with
  `WithDeletedAt`). The default Response is `204 No Content`.
- **`OpAction`** — The trimmed pipeline runs `Auth → action middleware →
  handler → Response`. The handler is responsible for its own body
  parsing, validation, and database calls.
- **`/openapi.json`** — A separate three-step pipeline (`OpenAPI.Auth → Generate → Response`)
  builds the spec from the registry every time, then writes the JSON
  document.

## What happens on errors

Three error paths at every step:

| Trigger | Effect |
|---|---|
| Middleware returns a non-nil `error` from `next()` | Bubbles up; later steps are skipped. The chain returns the error to the handler, which logs and writes a `500 INTERNAL` envelope. |
| Middleware calls `ctx.Abort(...)` and returns nil without `next()` | `ctx.Response` is set; subsequent steps are skipped (because `next()` was never called); the Response step's default reads `ctx.Response` and writes it. |
| Panic anywhere in the chain | `PanicRecoverer` catches it, logs through `Config.PanicLogger`, writes a `500 PANIC` envelope. |

A transaction in flight is rolled back by `WithTransaction`'s deferred
`Rollback` in all three cases — the same code path that handles success.

## What this tour did not show

- Multi-tenant scoping with `db.Tenancy` — would have appended to
  `ctx.Query.Filters` in step 5/6.
- `mfx:"file"` uploads — would have parsed multipart in step 3 and written
  bytes to `FileStorage` between steps 5 and 6.
- `mfx:"scheduled"` — fires outside the request, in a separate
  [Scheduled Runner](../advanced-topics/scheduled.md) goroutine.
- `mfx:"versioned"` — would have written a sibling history row in step 6's
  After phase, in the same transaction.
- The OpenAPI pipeline — same shape, three steps, separate registrations.

Each of those is covered in its own page; the lifecycle in step-by-step
form is the same.
