# OpenAPI Spec

maniflex generates an OpenAPI 3.1 specification from the registered models and
serves it at **`GET /openapi.json`**. The spec is derived from the same
struct tags that drive validation and querying, so it cannot drift from the
actual behaviour of the API.

## The endpoint

The spec lives at `/openapi.json` under the configured `PathPrefix`:

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
- **Error response shapes** for `400`, `404`, `409`, `422`, and `500`.

`hidden` fields are excluded entirely from every schema. `writeonly` fields
appear in the create and update schemas with `writeOnly: true`, but not in the
response shape.

## Schemas for custom types

Field types are mapped to OpenAPI schemas by their Go kind: strings, booleans,
the integer and float families, `time.Time` (as `date-time`), and any
string-keyed `map` (as a free-form `object`). A field whose type falls outside
these — a custom struct, or any type with a non-obvious JSON representation — is
**omitted** from the generated schema rather than guessed at.

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
| **Auth** | Same shape as the model-route Auth step. Gate `/openapi.json` here. |
| **Generate** | Builds the spec from the registry. After-position middleware mutates it. |
| **Response** | Serialises the spec to JSON. |

This is reached via `server.Pipeline.OpenAPI.*`. See
[OpenAPI Middleware](../middleware-catalogue/openapi.md) for the catalogue of
spec-shaping helpers — `SetTitle`, `AddServer`, `AddSecurityScheme`,
`AddExtension`.

## Securing the spec

The endpoint is public by default. To restrict it — say, to internal users
only — register an Auth middleware on the OpenAPI pipeline:

```go
import "github.com/xaleel/maniflex/middleware/auth"

server.Pipeline.OpenAPI.Auth.Register(
    auth.JWTAuth("my-secret"),
)
server.Pipeline.OpenAPI.Auth.Register(
    auth.RequireRole("internal"),
)
```

These run only for `/openapi.json`; they do not affect the model routes.

## Viewing the spec

The framework ships a Scalar API Reference viewer at
[`static/openapi.html`](../defining-your-api/static-files.md). When `static/` is present in the
working directory, it is served at `http://localhost:8080/static/openapi.html`
and loads `/api/openapi.json` directly — no extra setup needed for human
browsing of the generated spec.

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
    ctx.Spec.Info.Contact = &maniflex.OASContact{
        Name:  "API team",
        Email: "api@example.com",
    }
    return nil
}, maniflex.After)
```

The full set of types (`OpenAPISpec`, `OASInfo`, `OASSecurityScheme`, …) is in
the `maniflex` package.
