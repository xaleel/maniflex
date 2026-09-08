# OpenAPI Spec

maniflex generates an OpenAPI 3.1 specification from the registered models. The
HTTP endpoint is private-by-default: the zero-value configuration does not mount
it. Explicitly publish it or place it behind a shared documentation access policy.
The spec is derived from the same struct tags that drive validation and querying,
so it cannot drift from the actual behaviour of the API.

## The endpoint

To publish the spec intentionally, opt in when constructing the server:

```go
server := maniflex.New(maniflex.Config{
    Documentation: maniflex.DocumentationConfig{Public: true},
})
```

It then lives at `/openapi.json` under the configured `PathPrefix`:

```bash
curl localhost:8080/api/openapi.json
```

The response is a full OpenAPI 3.1 document — `info`, `paths`, `components`,
the lot. It updates automatically every time you register a model, change a
tag, or alter `maniflex.Config`. There is no separate codegen step.

## What is generated

For every registered model, the spec includes:

- **Five paths** — `/<table>` (`GET`, `POST`) and `/<table>/{id}` (`GET`,
  `PATCH`, `DELETE`).
- **One attachment path per `mfx:"file"` field** when storage is configured:
  `GET /<table>/{id}/<file_field>` with `application/octet-stream` (plus any
  MIME types from the field's `accept:` list). See
  [Per-model attachment routes](../defining-your-api/files.md#per-model-attachment-routes).
- **Every opt-in route the model actually mounts**, and only those — each
  appears when the `ModelConfig` flag that mounts it is set, and is absent
  otherwise:

  | Path | Mounted when |
  |---|---|
  | `GET /<table>/export` | `ExportEnabled` |
  | `GET /<table>/aggregate` | `AggregateEnabled` |
  | `POST /<table>/{id}/restore` | `RestoreEnabled` **and** the model soft-deletes |
  | `GET /<table>/{id}/history` | `Versioned` |
  | `POST /<table>/<file_field>/upload-url` | the field is `mfx:"file,upload:presigned"` and storage is configured |

  The condition is the same one the router checks, and
  `TestOpenAPIRouteParity` walks the mounted routes to prove it: a route the
  spec omits and a path the spec invents both fail the build. That guard exists
  because four of these routes shipped mounted-but-undocumented, so a generated
  client could not reach an endpoint that had worked for releases.
- **Three schemas** — a full response shape, a create body shape, and an
  update (patch) body shape. The three differ by which fields are visible: the
  create shape drops `readonly` fields; the update shape additionally drops
  `immutable` fields.
- **Standard query parameters** for list endpoints — `page`, `limit`,
  `filter`, `sort`, `include`.
- **Field metadata** taken from `mfx:` tags — `enum`, `min`, `max`,
  `required`, `readOnly`, `writeOnly`.
- **JSON / map fields** — a field whose type is a `map` with string keys is
  documented as a free-form `{"type": "object"}`. This covers `map[string]any`,
  `map[string]string`, and named types over them such as
  `type JSONObject map[string]any` — the value type is not inspected. To emit a
  more precise shape instead, give the type its own schema (see
  [Schemas for custom types](#schemas-for-custom-types)).
- **Relation fields** — for each relation to a *registered* model, the full
  response schema embeds the related schema by reference (`$ref`), shown when
  `?include=` requests it. A relation whose target model is not registered —
  for example a bare `RelatedID` field with no `Related` model — is omitted
  rather than emitting a dangling reference that would break spec validators
  and client generators.
- **Response statuses** beyond the obvious ones, derived from what this
  deployment can actually answer with — see
  [Statuses the pipeline produces](#statuses-the-pipeline-produces).

`hidden` fields are excluded entirely from every schema. `writeonly` fields
appear in the create and update schemas with `writeOnly: true`, but not in the
response shape.

## Statuses the pipeline produces

An operation's own shape gives it the obvious statuses: `201` and `422` on a
create, `404` on a read. Everything else a request can meet comes from the
pipeline and the configuration, so it is derived per operation rather than
assumed:

| Status | Documented when |
|---|---|
| `401`, `403` | any `Pipeline.Auth` middleware applies to that model and operation |
| `409` | a write on a model with a unique field or a relation; a delete on a model with a `restrict` relation |
| `412` | `ModelConfig.OptimisticLock`, on `PATCH` and `DELETE` |
| `500` | always |
| `503` | `Config.MaxConcurrentRequests` is set — carries a `Retry-After` header |
| `504` | `Config.QueryTimeout` is set |

Auth is not special-cased: a middleware on the `Auth` step implies `401`/`403`
because refusing is what that step is for, so `auth.JWTAuth` needs no OpenAPI
awareness of its own. A server with no auth middleware documents neither — which
is the point. A fixed list would tell clients to handle a `401` that cannot
happen, and a reviewer reading the spec as a security inventory would be misled
in one direction or the other.

With `OptimisticLock` set, both halves of the conditional-write handshake are
documented: the item `GET` carries an `ETag` response header, and `PATCH` /
`DELETE` take an optional `If-Match` request header. Optional because a request
without it writes unconditionally rather than failing.

### Statuses your own middleware produces

The framework cannot know that your guard answers `402`. Declare it on the
registration, where it inherits the same `ForModel` / `ForOperation` filters that
decide where the middleware runs:

```go
server.Pipeline.Validate.Register(stockGuard,
    maniflex.ForModel("Order"),
    maniflex.ForOperation(maniflex.OpCreate),
    maniflex.DocumentsResponse(409, "Out of stock", nil),
)
```

That `409` appears on `POST /orders` and nowhere else. A declaration replaces the
derived entry for the same status, so this is also how to say what your `409`
means rather than accepting the generic wording.

For a status no registered middleware produces — one a handler returns itself, or
one a proxy in front of the server can return — use `openapi.AddResponse`:

```go
server.Pipeline.OpenAPI.Generate.Register(
    openapi.AddResponse(
        openapi.OperationTarget{Path: "/orders", Method: "post"},
        402, "Payment required", nil),
    maniflex.After,
)
```

Prefer `DocumentsResponse` when a middleware you register is what produces the
status. The `Path` above is a literal, so it stops matching silently if the
model's table name or the route ever changes.

## Schemas for custom types

Field types are mapped to OpenAPI schemas by their Go kind: strings, booleans,
the integer and float families, `time.Time` (as `date-time`), any string-keyed
`map` (as a free-form `object`), and slices and arrays (as `array`, with the
element type inferred — except a byte slice, which `encoding/json` base64s and
which is therefore documented as `{"type": "string", "format": "byte"}`).

A field whose type falls outside these — a custom struct, or any type with a
non-obvious JSON representation — is published **with no type constraint**, and
the server says so at startup: a warning naming the model and the field, and a
startup error under `Config.Strict`.

Such a field is not omitted. It used to be, and a spec that omits a field says
two false things about it. A generated client has no type for it — and because
the omission happened before the `required` list was built, a field the server
*demands* was absent from `required` as well, so the spec described a request
that always fails. An unconstrained schema says less than a real one, but
everything it says is true.

To document such a type, make it implement the `ObjectWithSchema` interface:

```go
type ObjectWithSchema interface {
    Schema() *maniflex.OASSchema
}
```

Whenever the generator encounters a field of that type it calls `Schema()` and
uses the returned value verbatim — taking precedence over the built-in kind
mapping, so this also lets you override the default `object` shape a JSON/map
column would otherwise get. Either a value or a pointer receiver works.

For example, to document a `Geo` JSON column as a structured object instead of a
free-form one:

```go
type Geo struct {
    Lat float64 `json:"lat"`
    Lng float64 `json:"lng"`
}

func (Geo) Schema() *maniflex.OASSchema {
    return &maniflex.OASSchema{
        Type: "object",
        Properties: map[string]*maniflex.OASSchema{
            "lat": {Type: "number", Format: "double"},
            "lng": {Type: "number", Format: "double"},
        },
    }
}
```

A `Geo` field now renders with that exact shape in the response, create, and
update schemas. A pointer field (`*Geo`) is additionally made nullable.

## Custom actions

[Actions](../advanced-topics/actions.md) — custom endpoints registered with
`server.Action` — are included in the spec alongside the generated model
routes. Each contributes its method, path, and any `{...}` path parameters
automatically. Fill in `ActionConfig.OpenAPI` to document request and response
bodies (inferred directly from Go structs), extra query parameters, security
requirements, and a long-form description. See
[Documenting an action in OpenAPI](../advanced-topics/actions.md#documenting-an-action-in-openapi).

## The OpenAPI pipeline

The spec endpoint has its own three-step pipeline, parallel to the model-route
pipeline:

```
OpenAPI.Auth → OpenAPI.Generate → OpenAPI.Response
```

| Step | Purpose |
|---|---|
| **Auth** | OpenAPI-specific middleware (`OpenAPIMiddlewareFunc`) for format-specific gates. |
| **Generate** | Builds the spec from the registry. After-position middleware mutates it. |
| **Response** | Serialises the spec to JSON. |

This is reached via `server.Pipeline.OpenAPI.*`. See
[OpenAPI Middleware](../middleware-catalogue/openapi.md) for the catalogue of
spec-shaping helpers — `SetTitle`, `AddServer`, `AddSecurityScheme`,
`AddExtension`.

## Securing generated documentation

Use the shared router-level documentation policy to protect both OpenAPI and
AsyncAPI. `AdaptAuth` safely bridges request-level authentication middleware;
the middleware shares one context, so `JWTAuth` can populate the identity used
by `RequireRole`:

```go
{{#include ../../../documentation_example_test.go:adapt-auth}}
```

This mounts generated documentation behind the policy; it does not affect model
routes. Existing `Pipeline.OpenAPI.Auth` middleware remains supported for
OpenAPI-only custom gates, but it takes `OpenAPIMiddlewareFunc`, not the
model-route `MiddlewareFunc` returned by `auth.JWTAuth` and `auth.RequireRole`.

The framework does not force wildcard CORS headers on specifications. Configure
cross-origin access explicitly through `Documentation.Middleware` or global
`HTTPMiddlewares`.

## Viewing the spec

The framework ships a Scalar API Reference viewer at
[`static/openapi.html`](../defining-your-api/static-files.md). Point `StaticDir`
at the directory holding it (`maniflex.Config{StaticDir: "static"}`) and it is
served at `http://localhost:8080/static/openapi.html`, loading `/api/openapi.json`
directly. Public documentation works without additional viewer configuration.
For protected documentation, configure the viewer to send credentials and protect
the static viewer itself through your edge proxy or global HTTP middleware.

For tooling integration, the JSON document at `/openapi.json` is consumable by
any OpenAPI 3.1-compatible client generator, mock server, or contract testing
framework.

## Customising the spec

Most customisation is one-line, through the
[OpenAPI Middleware](../middleware-catalogue/openapi.md) helpers. For deeper edits, write
your own middleware:

```go
server.Pipeline.OpenAPI.Generate.Register(func(ctx *maniflex.OpenAPIContext, next func() error) error {
    if err := next(); err != nil {
        return err
    }
    // ctx.Spec is the just-generated *OpenAPISpec — mutate freely.
    ctx.Spec.Info.Description = "Contact the API team at api@example.com."
    return nil
}, maniflex.After)
```

The full set of types (`OpenAPISpec`, `OpenAPIInfo`, `OASSecurityScheme`, …) is in
the `maniflex` package.
