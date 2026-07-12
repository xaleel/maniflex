# Pipeline Overview

Every HTTP request handled by a generated model route flows through the same
six-step pipeline. Each step has a default behaviour supplied by the framework
and a registry of user middleware that can run before, after, or in place of
it. This page describes what each step is responsible for; later pages cover
how to register middleware on them and how state flows between them.

## The six steps

```
Auth → Deserialize → Validate → Service → DB → Response
```

| Step | Default behaviour |
|---|---|
| **Auth** | Pass-through. Populates nothing by default. User middleware sets `ctx.Auth` here. |
| **Deserialize** | Parses URL query parameters (`page`, `limit`, `filter`, `sort`, `include`) into `ctx.Query`. On `POST`/`PATCH`, reads the JSON body into `ctx.ParsedBody` (limit: 4 MB), or parses `multipart/form-data` into `ctx.ParsedBody` and `ctx.Files`. |
| **Validate** | For create and update, enforces the `mfx:` tag rules: strips `readonly` and `id`, rejects `immutable` on update, checks `required`, `enum`, `min`, `max`. |
| **Service** | Pass-through. Reserved for business logic supplied by user middleware. |
| **DB** | Dispatches to the configured adapter for the current operation — `FindMany`, `FindByID`, `Create`, `Update`, or `Delete`. Routes through `ctx.Tx` when a transaction is active. |
| **Response** | Builds the JSON envelope from `ctx.DBResult` and writes it to the `http.ResponseWriter`. |

The OpenAPI endpoint (`GET /openapi.json`) has its own three-step pipeline —
`Auth → Generate → Response` — accessible via `server.Pipeline.OpenAPI`. The
model-route pipeline described here is the one used for everything else.

## Operations

The CRUD operation a request performs is identified by an `Operation` value
that is stable across all six steps:

| Operation | Triggered by |
|---|---|
| `OpList` | `GET /<table>` |
| `OpRead` | `GET /<table>/{id}` |
| `OpCreate` | `POST /<table>` |
| `OpUpdate` | `PATCH /<table>/{id}` |
| `OpDelete` | `DELETE /<table>/{id}` |
| `OpOptions` | `OPTIONS /<table>` or `OPTIONS /<table>/{id}` |
| `OpReadAttachment` | `GET /<table>/{id}/<file_field>` — per-model attachment download (see [Files](../defining-your-api/files.md#per-model-attachment-routes)) |
| `OpAction` | a custom action endpoint registered with `server.Action()` |

A `HEAD` request has no operation of its own: it is `GET` with the body
suppressed, so it runs as `OpList` (collection) or `OpRead` (item) — same
middleware, same status code, no body. The `OpHead` constant still exists but is
never set on a request; scope HEAD-aware middleware with `OpRead` / `OpList`.

`ctx.Operation` is the value middleware uses to branch behaviour. `OpAction`
requests follow a trimmed pipeline (`Auth → action handler → Response`); the
Deserialize, Validate, Service, and DB steps are skipped for them.

## Per-step responsibilities

### Auth

The Auth step is the place to verify a token, look up a user, and set
`ctx.Auth`. The default handler does nothing; an unauthenticated request reaches
the DB layer with `ctx.Auth == nil`. Add a middleware here to reject anonymous
callers, populate identity, or check scopes.

### Deserialize

The Deserialize step assembles request input from three sources:

- The URL query string becomes a `*QueryParams` on `ctx.Query`. Filter and sort
  references are validated against the model's tag-derived field lists.
- A JSON body becomes `ctx.ParsedBody` (a read-only `*RequestBody`, JSON-keyed)
  and is bound to the typed record `ctx.Record`. Bodies over 4 MB are rejected
  as `BODY_READ_ERROR`.
- A multipart body populates both `ctx.ParsedBody` (the form fields) and
  `ctx.Files` (the file parts). The form-field-to-file-field mapping is by
  name.

Reads carry no body, so only `ctx.Query` is populated for `OpList` / `OpRead`.

### Validate

The Validate step runs only on `OpCreate` and `OpUpdate`. It applies the rules
declared by `mfx:` tags to `ctx.ParsedBody`:

- `readonly` fields and the `id` column are silently stripped.
- `immutable` fields are stripped on update.
- `required` fields must be present on create.
- `enum`, `min`, `max` are checked when the value is present.

Validation failures abort the pipeline with `422 Unprocessable Entity` and a
`details` payload listing every offending field.

### Service

The Service step has no default behaviour — it exists for application logic.
Hashing a password before persistence, charging a payment, recomputing a
derived total, calling an external API: all of these belong here. A Service
middleware that needs to short-circuit the request calls `ctx.Abort(...)` and
returns without invoking `next()`.

### DB

The DB step is the only step with side effects on the database. It selects the
operation matching `ctx.Operation`, builds the column-keyed write set from
`ctx.Record` (falling back to `ctx.ParsedBody`), calls the adapter, and writes
the result into `ctx.DBResult` — a `*ListResult` for lists, otherwise the record
(a typed `*T` on reads). When `ctx.Tx` is set, the call is routed through the
transaction; otherwise the bare adapter is used.

Two error classes are normalised at this step:

- `maniflex.ErrNotFound` becomes `404 NOT_FOUND`.
- `*maniflex.ErrConstraint` becomes `409 CONFLICT`.

A cancelled context becomes `504 TIMEOUT` — unless the cancellation came from the
client hanging up, which is `499` with no body (see
[Error Handling](errors.md)). All other adapter errors surface as
`500 DATABASE_ERROR`.

### Response

The Response step serialises `ctx.DBResult` into an `APIResponse` and writes it
to the wire. List responses include a `meta` block with `total`, `page`,
`limit`, and `pages`; single-record responses do not. The standard envelope is
`{"data": ...}` for success and `{"error": {...}}` for failure.

## Short-circuiting

Any middleware can stop the pipeline by setting `ctx.Response` (typically via
`ctx.Abort(status, code, message)`) and returning without calling `next()`.
Subsequent steps are skipped and the Response step writes the prepared error
envelope. This is the standard mechanism for unauthorised requests, validation
failures inside Service middleware, and any other refusal that should not
reach the database.

## Per-step middleware

Each step exposes a `*StepRegistry` on `server.Pipeline`:

```go
server.Pipeline.Auth.Register(jwtAuth)
server.Pipeline.Service.Register(hashPassword,
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
```

Registered middleware can run **Before** the default (the default position),
**After** it, or **Replace** it entirely. Scoping by model and operation is
covered in [Writing Middleware](middleware.md).

## Next

- **[ServerContext](context.md)** — the object threaded through every step.
- **[Writing Middleware](middleware.md)** — `Register`, options, positions.
- **[Transactions](transactions.md)** — wrapping the DB step in a transaction.
- **[Error Handling](errors.md)** — the error envelope and sentinel errors.
