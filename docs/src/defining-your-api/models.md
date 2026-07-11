# Models & BaseModel

A _model_ is a Go struct registered with the server. From it, maniflex derives a
database table, the JSON request and response shapes, the set of REST routes,
and the validation applied to every write. This page covers what a struct must
contain to be a valid model, how it maps to a table, and the options available
at registration. Field-level tags are documented in
[Field Tags Reference](tags.md); relationships in [Relations](relations.md).

## Definition

A model is an ordinary struct that embeds `maniflex.BaseModel`:

```go
type Article struct {
    maniflex.BaseModel
    Title string `json:"title" mfx:"required,filterable,sortable"`
    Body  string `json:"body"  mfx:"required"`
}
```

Registration validates the struct and adds it to the registry:

```go
server.MustRegister(Article{})
```

`Register` returns an error; `MustRegister` panics on failure and is intended
for use in `main` or package initialisation. A struct is rejected at
registration if it is not a struct type or does not embed `BaseModel`.

## BaseModel

Every model must embed `maniflex.BaseModel`. It contributes three columns common to
all tables:

```go
type BaseModel struct {
    ID        string    `json:"id"         db:"id"`
    CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly,sortable"`
    UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly,sortable"`
}
```

- **`ID`** — the primary key, a UUID assigned by the framework on create.
- **`CreatedAt`** — set once, when the row is created.
- **`UpdatedAt`** — refreshed on every update.

All three are managed by the framework. `CreatedAt` and `UpdatedAt` are
`readonly`: values supplied for them in a request body are ignored rather than
stored. Because they are part of `BaseModel`, they are never declared on
individual models.

A struct that does not embed `BaseModel` — or otherwise lacks an `id` column —
fails registration.

## Field mapping

Each exported field of a model maps to a database column. Three struct tags
control the mapping:

| Tag        | Purpose                                                           |
| ---------- | ----------------------------------------------------------------- |
| `json`     | the field's name in request and response bodies                   |
| `db`       | the column name; defaults to the snake_case field name if omitted |
| `maniflex` | field behaviour — validation, filterability, and so on            |

A minimal field needs only a `json` tag; `db` is derived and `mfx` is optional.
The `mfx` tag is the largest of the three and has its own reference in
[Field Tags Reference](tags.md). Fields that name a related model — for example
a `UserID` foreign key — are interpreted as relations; see
[Relations](relations.md).

## Table names

By default the table name is the struct name converted to snake_case and
pluralised:

| Struct     | Table        |
| ---------- | ------------ |
| `Article`  | `articles`   |
| `BlogPost` | `blog_posts` |
| `Category` | `categories` |

To use a different name, pass a `ModelConfig` with `TableName` set when
registering:

```go
server.MustRegister(
    Article{}, maniflex.ModelConfig{TableName: "articles"},
)
```

## Registration options

`ModelConfig` carries per-model options. All fields are optional; an omitted
`ModelConfig` applies the defaults described above.

| Field               | Purpose                                                                                                                  |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `TableName`         | override the derived table name                                                                                          |
| `SoftDelete`        | opt the model into soft deletion — see [Soft Delete](soft-delete.md)                                                     |
| `Middleware`        | pipeline middleware scoped to this model, installed at registration — see [Writing Middleware](../the-request-pipeline/middleware.md)            |
| `Versioned`         | record field-change history in a sibling `{model}_history` table                                                         |
| `VersionedDiffOnly` | with `Versioned`, store only changed fields rather than full snapshots                                                   |
| `Indices`           | additional database indexes created during `AutoMigrate`                                                                 |
| `ExportEnabled`     | mount `GET /:model/export` (CSV / XLSX) — see [CSV / XLSX Export](../advanced-topics/export.md)                                    |
| `MaxExportRows`     | row cap for the export endpoint; default 100,000                                                                         |
| `AggregateEnabled`  | mount `GET /:model/aggregate` (grouped count/sum/avg/min/max) — see [Aggregations](../advanced-topics/raw-queries.md#auto-generated-aggregate-endpoint) |
| `OptimisticLock`    | enable `If-Match` / ETag concurrency control on PATCH and DELETE                                                         |
| `Adapter`           | route this model to a separate database adapter                                                                          |
| `Singleton`         | expose the model as a single-row resource (`GET` / `PATCH`, no id) — see [Singleton models](#singleton-models-singleton) |
| `Headless`          | register the model fully but mount no REST routes, freeing its path for a custom action — see [Serving a model's own path from an action](../advanced-topics/actions.md#serving-a-models-own-path-from-an-action) |

### Optimistic locking (`OptimisticLock`)

When `OptimisticLock: true`, every PATCH and DELETE request that includes an
`If-Match` header is checked against the current record's ETag before the write
executes. A mismatch returns **412 Precondition Failed** (`PRECONDITION_FAILED`).
Requests without `If-Match` are unaffected — the flag opts in to enforcement,
not mandatory locking.

The ETag format is identical to the one emitted by `response.Cache` (MD5 of the
JSON response body), so clients can use the header from a preceding GET directly:

```go
server.MustRegister(Invoice{}, maniflex.ModelConfig{OptimisticLock: true})

server.Pipeline.Response.Register(
    response.Cache(300),
    maniflex.ForModel("Invoice"),
    maniflex.ForOperation(maniflex.OpRead),
    maniflex.AtPosition(maniflex.After),
)
```

```
GET  /invoices/42          → 200  ETag: "d41d8cd9..."
PATCH /invoices/42         If-Match: "d41d8cd9..."  → 200
PATCH /invoices/42         If-Match: "stale"        → 412
```

The check and the write it guards run as a single transaction, with the record
held under a row lock (`SELECT … FOR UPDATE` on Postgres) from the ETag
comparison until the write commits. Two clients holding the same ETag therefore
cannot both succeed: the loser waits on the lock, then re-reads a record whose
ETag has moved on and gets its 412. When the request already runs inside a
transaction (`maniflex.WithTransaction`) the guard joins it and the lock is held
until that transaction commits; otherwise the DB step opens and commits one of
its own.

### Singleton models (`Singleton`)

Some resources are inherently single-row: an application config record, a set of
feature flags, the banner an admin edits and every client reads at launch. With
`Singleton: true` the model drops its collection and item routes and exposes just
two endpoints on the bare table path — no id in the URL:

```
GET   /:model   → read the one row
PATCH /:model   → update the one row
```

There is no `POST`, `DELETE`, or list endpoint; requesting them returns
**405 Method Not Allowed**, and there is no `/:model/:id` subtree.

The single backing row is provisioned lazily under the well-known
`maniflex.SingletonID` on first access, from each column's default. So the first
`GET` returns defaults before anything has been written, and `PATCH` always
targets an existing row — it behaves like an upsert:

```go
type AppConfig struct {
    maniflex.BaseModel
    MaintenanceMode bool   `json:"maintenance_mode" mfx:"default:false"`
    MinAppVersion   string `json:"min_app_version"  mfx:"default:1.0.0"`
    Banner          string `json:"banner"`
}

server.MustRegister(
    AppConfig{}, maniflex.ModelConfig{Singleton: true, TableName: "config"},
)
```

```
GET   /config                                  → 200  {"data":{"id":"singleton","maintenance_mode":false,"min_app_version":"1.0.0","banner":""}}
PATCH /config   {"maintenance_mode": true}     → 200  {"data":{"id":"singleton","maintenance_mode":true, ...}}
GET   /config                                  → 200  (reflects the update)
POST  /config                                  → 405
```

Because the row is auto-provisioned from column defaults, a singleton model may
not declare `mfx:"required"` fields — there would be no value to satisfy them on
first access. Such a model is rejected at registration. Give fields sensible
`mfx:"default:…"` values (or make them pointers) instead.

### `ModelConfig` registration order

A `ModelConfig` is positioned immediately after the model it configures:

```go
server.MustRegister(
    User{},
    Article{}, maniflex.ModelConfig{Versioned: true},
    Comment{},
)
```

Here `User` and `Comment` use defaults; only `Article` is versioned.

Two argument shapes are detected and logged as a warning (they're foot-guns,
not errors yet — strict mode will promote them to a panic):

- A `ModelConfig` at position 0 (no preceding model to attach to).
- Two `ModelConfig`s in a row (only the first applies to the model; the
  second has no fresh model to bind to and is dropped).

## Optional embeds

Beyond `BaseModel`, the framework provides embeds that add columns and switch on
behaviour when present:

| Embed                    | Adds                              | Effect                      |
| ------------------------ | --------------------------------- | --------------------------- |
| `maniflex.WithDeletedAt` | `deleted_at` (nullable timestamp) | timestamp-based soft delete |
| `maniflex.WithIsDeleted` | `is_deleted` (boolean)            | flag-based soft delete      |

Embedding one of these is equivalent to setting `SoftDelete` in `ModelConfig`.
The two approaches and their query semantics are covered in
[Soft Delete](soft-delete.md).

```go
type Article struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt          // DELETE marks deleted_at instead of removing the row
    Title string `json:"title" mfx:"required"`
}
```

## Registration order

Models must be registered before the database adapter is opened. The adapter is
constructed from the registry — it reads the registered models to run
migrations and resolve relations — so the registry must be complete first:

```go
server.MustRegister(User{}, Article{}, Comment{})   // 1. populate the registry
db, err := sqlite.Open("./app.db", server.Registry()) // 2. build the adapter from it
server.SetDB(db)                                      // 3. inject the adapter
```

Registering a model after `SetDB` has no effect on an already-open adapter.

## Next

- **[Field Tags Reference](tags.md)** — every `mfx:` tag and its meaning.
- **[Relations](relations.md)** — foreign keys and slice fields.
- **[Soft Delete](soft-delete.md)** — `WithDeletedAt`, `WithIsDeleted`, and query behaviour.
