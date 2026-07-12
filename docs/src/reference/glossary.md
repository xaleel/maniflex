# Glossary

Every framework term used in these docs, in one place. Links point to the
page where each term is defined in depth.

## A

**Action.** A custom endpoint registered with `server.Action(...)`. Runs a
trimmed pipeline (`Auth → action middleware → handler → Response`); the
Deserialize, Validate, Service, and DB steps are skipped. See
[Custom Endpoints](../advanced-topics/actions.md).

**Adapter.** An implementation of the `maniflex.DBAdapter` interface. Two ship:
`db/sqlite` and `db/postgres`. Custom backends implement the same
interface. See [Database Backends](../deployment/databases.md).

**After (position).** A middleware position that places the function after
the step's default handler. The middleware sees the result of the default
(`ctx.DBResult`, `ctx.Response`). See [Writing Middleware](../the-request-pipeline/middleware.md).

**AuditRecord.** The structured record produced by `db.AuditLog` — fields
include `Timestamp`, `Model`, `Operation`, `ResourceID`, `Actor`,
`TenantID`, `RequestID`, `TraceID`, `ServiceName`, and optionally a
per-field `Changes` diff. See [Audit Logging](../advanced-topics/audit.md).

**AuthInfo.** The struct populated by Auth middleware and stored on
`ctx.Auth`. Carries `UserID`, `Roles`, `Claims`, `TenantID`,
`IdentityType`, `Scopes`, `SessionID`, `AuthMethod`. See
[ServerContext](../the-request-pipeline/context.md).

**AutoMigrate.** The startup phase that creates and alters tables to match
registered models. Runs by default; disable with `Config.DisableAutoMigrate`. See
[Database Backends](../deployment/databases.md).

## B

**BaseModel.** The struct every registered model must embed. Contributes
`id` (UUID, framework-assigned), `created_at`, and `updated_at`. See
[Models & BaseModel](../defining-your-api/models.md).

**Batch.** Multiple inserts or updates inside a single request, usually
through a [custom action](../advanced-topics/actions.md) that opens one transaction.
See [Batch Operations & Sagas](../advanced-topics/batch-saga.md).

**Before (position).** The default middleware position; the function runs
before the step's default handler. See [Writing Middleware](../the-request-pipeline/middleware.md).

**BelongsTo.** A relation where this model carries the foreign key. Declared
either by convention (`UserID` field → `User`) or explicitly with
`mfx:"relation:Name"` plus a companion field of the target type. See
[Relations](../defining-your-api/relations.md).

## C

**Cache (`CacheStore`).** The generic key/value cache interface used by
several middlewares (idempotency, rate limit, response cache). `maniflex`
ships `MemoryCache`; satellite modules provide Redis-backed
implementations. See `cache.go`.

**Catalogue.** The set of ready-made middleware shipping under
`maniflex/middleware/`. See [Middleware Catalogue](../middleware-catalogue/index.md).

**ServerContext.** The single per-request struct threaded through every
pipeline step. Fields include `Request`, `Writer`, `Ctx`, `Model`,
`Operation`, `ResourceID`, `RawBody`, `ParsedBody`, `Query`, `DBResult`,
`Response`, `Auth`, `Tx`. See [ServerContext](../the-request-pipeline/context.md).

**Companion field.** On an explicit `BelongsTo` relation, the struct field
of the target type that accompanies the FK column. Required by
`mfx:"relation:Name"`; the field is named `Name` and typed as the target
model. See [Relations](../defining-your-api/relations.md).

**Config (`maniflex.Config`).** The single struct passed to `maniflex.New`. Every
field has a sensible default. See [Configuration](../deployment/config.md).

## D

**DBAdapter.** The interface implemented by every database backend.
Methods: `FindByID`, `FindMany`, `Create`, `Update`, `Delete`, `BeginTx`,
`Raw`, `Ping`. See [Database Backends](../deployment/databases.md).

**Diff (versioning).** The per-field `{old, new}` map written into the
`diff` column of a history row. Hidden, writeonly, and encrypted fields
are excluded. See [Versioning & History](../advanced-topics/versioning.md).

## E

**Embed.** A `BaseModel`, `WithDeletedAt`, or `WithIsDeleted` value
included in a model struct as an anonymous field. Embeds contribute
columns and turn on framework behaviour. See [Models & BaseModel](../defining-your-api/models.md)
and [Soft Delete](../defining-your-api/soft-delete.md).

**Envelope (storage).** The binary blob produced by `KeyProvider.Encrypt`,
stored in the database as `enc:<base64(envelope)>`. The envelope embeds
its `keyID` so `Decrypt` can route to the right key. See
[Encryption at Rest](../advanced-topics/encryption.md).

**Envelope (response).** The JSON shape `{"data": ...}` (success) or
`{"error": {...}}` (failure). Customisable via `response.Envelope`. See
[Response Envelope](../using-the-api/responses.md).

## F

**FieldMeta.** The framework's per-field metadata, derived from a struct
field's `json` / `db` / `mfx` tags by `parseFieldTags`. Stored on
`ModelMeta.Fields`. See [Models & BaseModel](../defining-your-api/models.md).

**FileStorage.** The interface for file backends. `maniflex/storage` ships
`LocalStorage`; custom implementations cover S3, R2, GCS. See
[File Fields & Uploads](../defining-your-api/files.md).

**Filter / `FilterExpr`.** A parsed `?filter=` expression. A slice of
these on `ctx.Query.Filters` becomes the SQL `WHERE` clause. See
[Querying](../using-the-api/querying.md).

## G

**Generate (OpenAPI step).** The middle step of the OpenAPI pipeline that
builds the `*OpenAPISpec` from the registry. After-position middleware
customises the spec. See [OpenAPI Spec](../using-the-api/openapi.md).

## H

**Handler (action).** The `func(ctx *maniflex.ServerContext) error` registered
with `server.Action(...)` as an action's body. See
[Custom Endpoints](../advanced-topics/actions.md).

**HasMany.** A relation declared as a slice field of the related struct.
No column on this table; the related table carries the FK. See
[Relations](../defining-your-api/relations.md).

**HMAC column.** A `{field}_hmac` companion column auto-created for
`mfx:"encrypted,unique"` fields, storing a keyed digest so uniqueness can
be enforced without comparing ciphertexts. See
[Encryption at Rest](../advanced-topics/encryption.md).

**History table.** The `{model}_history` sibling table created for a
`maniflex.ModelConfig{Versioned: true}` model. Receives one row per write to
the source model. See [Versioning & History](../advanced-topics/versioning.md).

## I

**Idempotency-Key.** The HTTP header consumed by
`middleware/idempotency` to deduplicate retries. The first request runs;
subsequent requests with the same key replay the cached response. See
[Idempotency](../advanced-topics/idempotency.md).

**IdentityType.** The `AuthInfo` field classifying the principal — `Human`,
`ServiceAccount`, or `Anonymous`. See [ServerContext](../the-request-pipeline/context.md).

**Immutable (tag).** A `mfx:` directive that accepts a value on create but
strips it on update. See [Field Tags Reference](../defining-your-api/tags.md).

**Include.** The `?include=relationKey,...` query parameter that populates
related rows inline in the response. See [Relations](../defining-your-api/relations.md) and
[Querying](../using-the-api/querying.md).

**Index (`IndexSpec`).** A declared database index, created during
`AutoMigrate`. Declared in `ModelConfig.Indices` or auto-generated for
`mfx:"scheduled"` columns. See `model.go`.

## J

**Junction (model).** The third model in a many-to-many relation, carrying
the two FKs. Named via `mfx:"through:JunctionModel"` on both sides. See
[Relations](../defining-your-api/relations.md).

## K

**KeyProvider.** The interface that the encryption subsystem uses to
encrypt, decrypt, and HMAC field values. Implementations: `EnvKeyProvider`,
`VaultKeyProvider`. See [Encryption at Rest](../advanced-topics/encryption.md).

## L

**`LockForUpdate`.** A `*ServerContext` method that acquires a row-level
write lock inside an active transaction. `SELECT ... FOR UPDATE` on
Postgres; transaction-level lock on SQLite. See [Transactions](../the-request-pipeline/transactions.md).

## M

**`MiddlewareFunc`.** The signature every pipeline middleware must
satisfy: `func(ctx *maniflex.ServerContext, next func() error) error`. See
[Writing Middleware](../the-request-pipeline/middleware.md).

**`ModelAccessor`.** The CRUD helper returned by `ctx.GetModel(name)`.
Exposes `List`, `Read`, `Create`, `Update`, `Delete` for any registered
model, routed through `ctx.Tx` when set. See [ServerContext](../the-request-pipeline/context.md).

**`ModelConfig`.** The per-model options passed alongside a struct in
`MustRegister`. Fields: `TableName`, `SoftDelete`, `Middleware`,
`Versioned`, `VersionedDiffOnly`, `Indices`. See [Models & BaseModel](../defining-your-api/models.md).

**`ModelMeta`.** The framework's runtime description of a registered
model. Carries `Name`, `TableName`, `Fields`, `Relations`, `SoftDelete`,
`Config`, `Indices`, and resolved scheduled specs.

## O

**OnDelete.** The referential action attached to a foreign key —
`cascade`, `setNull`, `restrict`, or unset. See [Relations](../defining-your-api/relations.md).

**Operation (`maniflex.Operation`).** The CRUD verb identifying a request:
`OpList`, `OpRead`, `OpCreate`, `OpUpdate`, `OpDelete`, `OpOptions`, `OpAction`.
A `HEAD` request runs as the `GET` it mirrors (`OpRead` / `OpList`). See
[Pipeline Overview](../the-request-pipeline/pipeline.md).

**Outbox.** The transactional outbox pattern: a row written in the same
transaction as the primary write, consumed by a background worker for
external side effects (emails, webhooks, events). See
[Batch Operations & Sagas](../advanced-topics/batch-saga.md).

## P

**Pipeline.** The six-step request pipeline (`Auth → Deserialize →
Validate → Service → DB → Response`) and its sibling OpenAPI pipeline.
See [Pipeline Overview](../the-request-pipeline/pipeline.md).

**Position.** Where in a step's chain a middleware sits — `Before`
(default), `After`, or `Replace`. See [Writing Middleware](../the-request-pipeline/middleware.md).

## Q

**Query model.** A read-only model registered with a SQL body
(`ModelConfig.QueryModel`) instead of a table. Generates filterable,
sortable list endpoints from a custom SELECT. See
[Raw Queries & Query Models](../advanced-topics/raw-queries.md).

**`QueryParams`.** The parsed `?page=&limit=&filter=&sort=&include=` of
a request, stored on `ctx.Query`. See [Querying](../using-the-api/querying.md).

## R

**Registry.** The in-memory map of every registered `*ModelMeta`. Built
by `MustRegister`, consumed by the adapter and the router. See
[Architecture](../how-it-works/architecture.md).

**Relation.** A connection between two models — `BelongsTo`, `HasMany`,
or `ManyToMany`. See [Relations](../defining-your-api/relations.md).

**Replace (position).** A middleware position that substitutes the step's
default handler entirely. The last matching `Replace` middleware wins.
See [Writing Middleware](../the-request-pipeline/middleware.md).

## S

**Saga.** A multi-step workflow composed of forward steps and
compensating undos, usually implemented via a transactional outbox and a
worker. See [Batch Operations & Sagas](../advanced-topics/batch-saga.md).

**Scheduled (tag / runner).** `mfx:"scheduled;..."` declares a
time-driven transition on a `*time.Time` column. The
`scheduled.Runner` sweeps the rows and applies the transition. See
[Scheduled Fields](../advanced-topics/scheduled.md).

**Service name.** `Config.ServiceName`. Identifies the service in logs,
audit records, and the `X-Service-Name` response header. See
[Configuration](../deployment/config.md).

**Soft delete.** Marking a row as deleted instead of removing it. Opt in
via `maniflex.WithDeletedAt` (timestamp) or `maniflex.WithIsDeleted` (boolean). See
[Soft Delete](../defining-your-api/soft-delete.md).

**Step (`StepRegistry`).** One of the six pipeline steps. Exposes
`Register(fn, opts...)` to attach middleware. See
[Pipeline Overview](../the-request-pipeline/pipeline.md).

## T

**Tag (`mfx:` directive).** A comma-separated list in a struct field's
`mfx` tag declaring per-field behaviour. See
[Field Tags Reference](../defining-your-api/tags.md).

**TenantID.** The `AuthInfo` field that scopes a principal to one tenant.
Populated by JWT auth (`TenantClaim`) or by custom Auth middleware. See
[Auth Middleware](../middleware-catalogue/auth.md).

**Through (tag).** `mfx:"through:JunctionModel"` declares a many-to-many
relation via a named junction model. See [Relations](../defining-your-api/relations.md).

**Trace (`PipelineTrace`).** Debug-level pipeline tracing controlled by
`Config.Trace`. Sub-flags: `Steps`, `Timings`, `Aborts`, `Bodies`,
`Skips`. See [Configuration](../deployment/config.md).

**Tx.** A transaction handle returned by `ctx.BeginTx` (or the underlying
adapter). Stored on `ctx.Tx`; the default DB step routes through it
automatically. See [Transactions](../the-request-pipeline/transactions.md).

## V

**Versioned (config).** `ModelConfig.Versioned = true` writes a row to
the sibling `{model}_history` table on every change to the source model.
See [Versioning & History](../advanced-topics/versioning.md).

## W

**`WithTransaction`.** The catalogue middleware that wraps the DB step in
a transaction, committing on success and rolling back on error. See
[Transactions](../the-request-pipeline/transactions.md).

**`writeonly`.** A tag directive that accepts the field on input but
strips it from responses. Standard choice for passwords. See
[Field Tags Reference](../defining-your-api/tags.md).
