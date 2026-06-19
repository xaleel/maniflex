# Architecture

This page explains how maniflex is put together. The reference pages describe
each piece in isolation; here we look at how they fit. A reader who finishes
this page should be able to point at any other doc and predict roughly what
it covers.

## The five pieces

```
                    ┌──────────────────────┐
        register →  │      Registry        │  ← models discovered here
                    └──────────────────────┘
                            │             │
                            │             ▼
                            │     ┌──────────────────────┐
                            │     │  Database adapter    │
                            │     │  (sqlite | postgres) │
                            │     └──────────────────────┘
                            ▼
                    ┌──────────────────────┐
                    │       Router         │  ← chi v5
                    └──────────────────────┘
                            │
                            ▼
HTTP request  →  ┌──────────────────────────────────────────────────┐
                 │  Pipeline:                                       │
                 │   Auth → Deserialize → Validate →                │
                 │   Service → DB → Response                        │
                 └──────────────────────────────────────────────────┘
                            │
                            ▼
HTTP response ← APIResponse envelope
```

The framework has **five primary moving parts**:

| Piece | What it is | Where it lives |
|---|---|---|
| **Registry** | An in-memory map of every registered model's `*ModelMeta` — fields, tags, relations, indices, scheduled specs. | built by `MustRegister`, consumed by the router and the adapter |
| **Router** | A chi v5 `Router` mounted with one sub-router per registered model. | `router.go` |
| **Pipeline** | Six ordered steps that every model-route request flows through, plus a parallel three-step pipeline for `/openapi.json`. | `pipeline.go` |
| **ServerContext** | The single per-request struct threaded through every step. | `context.go` |
| **DBAdapter** | The backend interface implemented by `db/sqlite`, `db/postgres`, and any custom backend. | `db.go` |

Every other concept in the docs sits on top of these five.

## Reflection, not codegen

The framework derives everything from the registered structs at startup:

1. `ScanModel` walks the struct with `reflect` once per model, builds a
   `*ModelMeta`, and inserts it into the registry.
2. The adapter reads the registry to emit `CREATE TABLE` / `ALTER TABLE`
   statements during `AutoMigrate`.
3. The router reads the registry to mount the five REST routes per model
   plus `/openapi.json`.
4. The Validate step reads each request's model meta to enforce `mfx:` tag
   rules.
5. The DB step reads each request's model meta to assemble the SQL.

Reflection runs **once at registration**, never per request. The per-request
path is allocation-light: a `map[string]any` for the body, a few string keys,
and the slice of registered middleware filtered by `ForModel` /
`ForOperation`. A model with N fields produces O(N) work at boot and O(N)
work per request, both linear in the size of the model.

This is the architectural difference from codegen frameworks: there is no
generated file to keep in sync. Changing a `mfx:` tag changes the runtime
behaviour the next time the process starts.

## The registry is the contract

Every other piece reads from the registry; nothing writes to it after
`Start()`. That single rule explains several constraints:

- **`MustRegister` must run before `sqlite.Open` / `postgres.Open`.** The
  adapter reads the registry during its constructor to learn about tables
  and relations.
- **Models cannot be added or removed at runtime.** New models require a
  process restart.
- **Models can be inspected from middleware** via `ctx.Model`, which is the
  `*ModelMeta` the router selected for this request.
- **Cross-model operations work** because middleware reaches into the
  registry through `ctx.GetModel(name)` or by name in scoped registration.

The framework's startup sequence is deliberate:

```
1. maniflex.New(cfg)            → empty registry
2. server.MustRegister(...) → populated registry
3. sqlite.Open(..., reg)    → adapter built from registry
4. server.SetDB(db)         → adapter wired into the DB step
5. middleware.Register(...) → pipeline customised
6. server.Start()           → router built from registry, listener opens
```

`Start()` runs `AutoMigrate` (if enabled) before opening the listener, so a
schema mismatch fails fast instead of corrupting writes.

## The pipeline is the unit of customisation

Every HTTP request to a model route is wrapped in a `ServerContext` and run
through six steps in this order:

| Step | Default behaviour |
|---|---|
| **Auth** | passthrough |
| **Deserialize** | parse query string + body |
| **Validate** | enforce `mfx:` tag rules |
| **Service** | passthrough — business logic goes here |
| **DB** | dispatch to the adapter |
| **Response** | write the JSON envelope |

Each step has its own `StepRegistry` on `server.Pipeline`. A registration
attaches a `MiddlewareFunc` at `Before` (the default), `After`, or `Replace`
position, scoped by `ForModel` / `ForOperation`. At request time the
registry returns the matching chain for the (model, operation) pair, the
chain runs, and any step can short-circuit by setting `ctx.Response` and
returning without calling `next()`.

The pipeline is the answer to "where do I put X?":

- **Identity checks** — Auth.
- **Coerce types, strip unknown fields** — Validate (Before).
- **Hash passwords, set derived fields** — Service.
- **Bracket the DB call** — DB (Before / After).
- **Webhooks, events, audit log** — DB (After).
- **Headers, redactions, metrics** — Response.

The same answer applies whether the code lives in a catalogue middleware,
a custom function, or a per-model `ModelConfig.Middleware`.

## The adapter is one interface

`maniflex.DBAdapter` has fewer than a dozen methods — `FindByID`, `FindMany`,
`Create`, `Update`, `Delete`, `BeginTx`, `Raw`, `Ping`, plus the schema
operations called by `AutoMigrate`. Two implementations ship:

- `db/sqlite` — pure-Go SQLite for development.
- `db/postgres` — `lib/pq` for production.

Both implementations share `db/sqlcore`, a SQL adapter that knows about
filters, sorts, includes, soft delete, and relations. A custom backend — an
HTTP data service, a different SQL database — implements the same interface
and is injected with `server.SetDB(myAdapter)`. No other code changes.

The same holds for `FileStorage` and `KeyProvider`: small interfaces with
shipped implementations and obvious extension points.

## Satellite modules

`maniflex` is a multi-module repository. The core module imports only chi and
uuid. Everything heavier — a database driver, a JWT library, a Kafka client,
bcrypt — lives in its own satellite under the same root, so a consumer
pulls in only the dependencies it actually imports.

The split keeps the core small and stable. It also keeps the trust boundary
clear: the framework's surface area is the `maniflex` package; everything in
`middleware/*`, `events/*`, `jobs/*`, `db/*` is application code that
happens to ship alongside the framework.

See [Satellite Modules](../deployment/modules.md) for the full layout and import rules.

## Two pipelines, one router

The router actually mounts **two** pipelines.

The first one — the six-step pipeline described above — handles
`/<table>` and `/<table>/{id}` for every registered model.

The second is a three-step pipeline for `GET /openapi.json`:

```
OpenAPI.Auth → OpenAPI.Generate → OpenAPI.Response
```

`Generate` derives the spec from the registry every time the endpoint is
hit, then `Response` serialises it. After-position middleware on `Generate`
can mutate the spec — change titles, add servers, install security schemes,
or rewrite arbitrary fields. See [OpenAPI Spec](../using-the-api/openapi.md) and the
[OpenAPI Middleware](../middleware-catalogue/openapi.md) catalogue.

A third, trimmed pipeline runs for [custom actions](../advanced-topics/actions.md):

```
Auth → [per-action middleware] → handler → Response
```

The Deserialize, Validate, Service, and DB steps are skipped — actions own
their body parsing and database work.

## Where each feature lives in the lifecycle

| Feature | Step(s) | Notes |
|---|---|---|
| `mfx:` tag rules (`required`, `enum`, `min`, …) | Validate | per-field |
| Required-on-create, immutable-on-update, readonly-strip | Validate | tied to `Operation` |
| `mfx:"file"` multipart parsing | Deserialize | populates `ctx.Files` |
| File storage write | Service (built-in) | writes the storage key |
| Soft-delete filter on reads | DB | adapter rewrites the SQL |
| `mfx:"encrypted"` envelope + HMAC | DB (Before for writes, after for reads) | needs `KeyProvider` |
| `Versioned` history row | DB (Before for pre-image, After for write) | sibling `_history` table |
| `mfx:"scheduled"` sweep | outside the request — separate runner | see [Scheduled Fields](../advanced-topics/scheduled.md) |
| `?filter=…&sort=…&include=…` parsing | Deserialize | into `ctx.Query` |
| `?include=` population | DB | secondary queries after the main `SELECT` |
| Auto-tenant filter (`db.Tenancy`) | DB (Before) | appends to `ctx.Query.Filters` |
| Audit log | DB (Before) | needs the pre-image; writes outside the request |
| `maniflex.WithTransaction` | Service (Before) or DB (Replace) | wraps the DB step |
| `LockForUpdate` | inside the DB step's transaction | `SELECT ... FOR UPDATE` on Postgres |

For a single concrete trace of every step running on one request, see the
[Request Lifecycle](lifecycle.md) walkthrough.

## What maniflex is not

To set expectations on the architecture choice:

- **Not codegen.** No generated files, no separate build step.
- **Not a router framework.** The HTTP layer is chi; maniflex uses it.
- **Not opinionated about JSON shape.** The envelope is the default, but
  `response.Envelope` lets you replace it. Errors always use the error
  envelope.
- **Not a service mesh.** One process, one binary. Multi-process concerns
  (events, jobs, distributed locks) live in the satellite modules.
- **Not magic.** Every behaviour is a function in the `maniflex` package or one
  of the catalogue middlewares. Read the source when in doubt; the pipeline
  is small.

## Next

- **[Request Lifecycle](lifecycle.md)** — a single `POST /api/orders`
  traced end-to-end through every step.
- **[Glossary](../reference/glossary.md)** — every framework term in one place.
- **[Pipeline Overview](../the-request-pipeline/pipeline.md)** — the per-step reference.
