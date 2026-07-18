# maniflex

**Annotated Go structs in. A full REST API out.**

maniflex is a Go framework that turns a plain struct into a production REST API —
filtering, pagination, relations, soft-delete, file uploads, and an OpenAPI 3.1
spec — with no generated code and no per-endpoint boilerplate. Behaviour is
declared with `mfx:` struct tags and customised through a composable
six-step middleware pipeline.

```go
type Post struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    Title  string `json:"title"  mfx:"required,filterable,sortable"`
    Body   string `json:"body"   mfx:"required"`
    Status string `json:"status" mfx:"required,filterable,enum:draft|published|archived"`
    UserID string `json:"user_id" mfx:"required,filterable"`
}
```

Register that struct and you get `GET/POST /posts`, `GET/PATCH/DELETE /posts/{id}`,
`?filter=status:eq:published`, `?sort=created_at:desc`, `?page=2&limit=20`,
`?include=user`, soft-delete semantics, and an entry in `/openapi.json`.

## Why maniflex

- **Reflection, not codegen.** Register a struct at startup; routes, schema, and
  validation are derived from it at runtime. Nothing to regenerate when a field
  changes.
- **One module, two dependencies.** The core `maniflex` module depends only on
  [chi](https://github.com/go-chi/chi) and [uuid](https://github.com/google/uuid).
  Postgres, Redis, Kafka, NATS, bcrypt and friends live in satellite modules you
  pull in only if you import them.
- **A pipeline you can reach into.** Every request flows through six ordered
  steps — Auth → Deserialize → Validate → Service → DB → Response. Hook
  middleware `Before`, `After`, or `Replace` at any step, scoped by model or
  operation.
- **Batteries included, swappable.** Ready-made middleware for JWT auth, unique
  validation, password hashing, multi-tenancy, audit logging, CORS, and more —
  each one just a function you can replace.
- **SQLite or Postgres.** Both backends share one SQL adapter. Develop against
  pure-Go SQLite (no CGo, no external service), deploy on Postgres.

## Core concepts

Five ideas carry the whole framework. Understand these and the rest of the docs
slot into place.

### Model

A Go struct that embeds `maniflex.BaseModel` and declares its fields with `mfx:`
struct tags. The struct is the single source of truth: it defines the database
table, the JSON request and response shapes, the validation rules, and which
fields are filterable or sortable. Optional embeds add behaviour — `maniflex.WithDeletedAt`
turns on soft-delete — and naming conventions like a `UserID` field declare
relations.
→ [Models & BaseModel](defining-your-api/models.md), [Field Tags Reference](defining-your-api/tags.md),
[Relations](defining-your-api/relations.md)

### Registry

The collection of every registered model, built by `MustRegister` at startup.
It is consumed in two places: the HTTP router reads it to mount routes, and the
DB adapter reads it to run migrations and resolve relations. This is why
`MustRegister` must run **before** `sqlite.Open` / `postgres.Open` — the adapter
is handed the populated registry.
→ [Getting Started](getting-started.md)

### Pipeline

Six ordered steps every request flows through: **Auth → Deserialize → Validate →
Service → DB → Response**. The pipeline is the unit of customisation — instead of
writing handlers, you attach middleware to the step where your logic belongs.
→ [Pipeline Overview](the-request-pipeline/pipeline.md), [ServerContext](the-request-pipeline/context.md)

### Middleware

A `func(ctx *maniflex.ServerContext, next func() error) error` registered on a pipeline
step. Registration is scoped with `maniflex.ForModel(...)` and `maniflex.ForOperation(...)`,
and positioned with `maniflex.AtPosition(maniflex.After)` (or `maniflex.Before`, the default, or `maniflex.Replace`). Set
`ctx.Response` and return without calling `next()` to short-circuit the request.
→ [Writing Middleware](the-request-pipeline/middleware.md), [Middleware Catalogue](middleware-catalogue/index.md)

### Adapter

The database backend implementing the storage interface. Two ship in-tree —
`db/sqlite` (pure-Go, no CGo) and `db/postgres` — and both share one SQL core.
Inject one with `server.SetDB(db)`, which patches the pipeline's DB step in
place.
→ [Database Backends](deployment/databases.md), [Transactions](the-request-pipeline/transactions.md)

## Where to go next

- **[Getting Started](getting-started.md)** — install, define your first model, run the server.
- **[Models & Tags](defining-your-api/models.md)** — the full `mfx:` tag reference and relation conventions.
- **[The Pipeline](the-request-pipeline/pipeline.md)** — how requests flow and where to hook in.
- **[Middleware Catalogue](middleware-catalogue/index.md)** — ready-made middleware for every step.
- **[Querying the API](using-the-api/querying.md)** — filtering, sorting, pagination, includes.

---

_maniflex requires Go 1.25 or newer._
