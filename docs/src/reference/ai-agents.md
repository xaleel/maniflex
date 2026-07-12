# AI Agents

A condensed, self-contained reference for AI coding agents working on maniflex
projects. Copy the block below into `CLAUDE.md`, `AGENTS.md`, or an equivalent
context file. Everything an agent needs to write correct maniflex code is in it;
no prose links to chase.

````md
# maniflex — Reference for AI coding agents

Reflection-driven Go REST framework. Annotated structs in → full REST API out:
filtering, pagination, relations, soft-delete, file uploads, OpenAPI 3.1 spec.
No codegen.

## Modules

- `maniflex` — core (chi + uuid only).
- `maniflex/db/sqlite` — pure-Go SQLite (modernc.org/sqlite). No CGo.
- `maniflex/db/postgres` — lib/pq.
- `maniflex/middleware/{auth,body,validate,service,db,response,openapi,idempotency}` — catalogue.
- `maniflex/middleware/service/bcrypt` — password hashing.
- `maniflex/middleware/db/redis` — Redis cache backend.
- `maniflex/events/{kafka,nats,rabbitmq,redis}` — event publishers.
- `maniflex/jobs/redis` — background job queue.
- `maniflex/scheduled` — runner for `mfx:"scheduled"` fields.
- `maniflex/storage` — local file storage; ships `LocalStorage`.
- `maniflex/pkg/encryption` — `EnvKeyProvider`, `VaultKeyProvider`.

Each is its own module. Import only what you use.

## The four-step lifecycle (fixed order)

```go
server := maniflex.New(maniflex.Config{Port: 8080, PathPrefix: "/api"})
server.MustRegister(User{}, Post{})                    // 1. populate registry
db, _ := sqlite.Open("./app.db", server.Registry())    // 2. adapter reads registry
server.SetDB(db)                                       // 3. inject adapter
log.Fatal(server.Start())                              // 4. serve
```

Models registered after `SetDB` do not reach the adapter. The adapter is built
from the registry at `Open` time.

## Models

Every model embeds `maniflex.BaseModel`:

```go
type Post struct {
    maniflex.BaseModel             // adds id (UUID), created_at, updated_at
    maniflex.WithDeletedAt         // optional — adds deleted_at (timestamp soft delete)
    // maniflex.WithIsDeleted      // alternative — adds is_deleted (bool)

    Title  string `json:"title"  mfx:"required,filterable,sortable"`
    Body   string `json:"body"   mfx:"required"`
    Status string `json:"status" mfx:"required,enum:draft|published"`
    UserID string `json:"user_id" mfx:"required,filterable"` // BelongsTo User
}
```

Table name = pluralised snake-case of struct name (`BlogPost` → `blog_posts`).
Override with `ModelConfig.TableName`.

Validation:
- Struct must be a struct type and embed `BaseModel`, else `MustRegister` panics.
- Field types: scalars, `*time.Time`, slices for relations, `map[string]any`,
  structs for companions.

## The `mfx:` tag — complete list

Comma-separated. Whitespace trimmed. Unknown directives ignored.

**Validation:**
- `required` — must be present on create
- `enum:a|b|c` — pipe-separated allowed values
- `min:N`, `max:N` — numeric bounds
- `default:V` — used when field absent on create (cast to type)

**Write access:**
- `readonly` — stripped from all writes
- `immutable` — accepted on create, rejected on update

**Response visibility:**
- `writeonly` — accepted on write, hidden in responses (e.g. password)
- `hidden` — hidden in responses **and** stripped from create/update schemas

**Query:**
- `filterable` — usable in `?filter=`
- `sortable` — usable in `?sort=`
- `searchable` — full-text search hint

**Schema:**
- `unique` — `UNIQUE` constraint at the column

**Relations** (semicolon-separated sub-options):
- `relation:Name` — explicit FK; requires companion field `Name` of target type
- `relation:Name;onDelete:cascade|setNull|restrict` — referential action
- `through:JunctionModel` — declares M2M via junction (on slice fields)

**Files:**
- `file` — multipart file field; column stores storage key
- `max_size:N` — `KB`/`MB`/`GB` suffix or bytes
- `accept:pattern1|pattern2` — MIME-type patterns
- `auto_delete:false` — keep stored file when row deleted or field replaced

**Encryption:**
- `encrypted` — AES-256-GCM at rest; not `filterable`/`sortable`
- `key:NAME` — keyID for KeyProvider (default `"default"`)
- `encrypted,unique` — adds `{field}_hmac` companion column for uniqueness

**Versioning** (on embedded `BaseModel` field):
- `mfx:"versioned"` — adds `{model}_history` sibling table
- `mfx:"versioned:diff_only"` — store diffs only, skip snapshots

**Scheduled** (on `*time.Time` fields):
- `scheduled;soft-delete` — needs `WithDeletedAt`/`WithIsDeleted`
- `scheduled;hard-delete`
- `scheduled;field=NAME;to=VALUE` — required pair
- `scheduled;field=NAME;from=OLD;to=NEW` — guarded transition

**Exclusion:** `json:"-"`, `db:"-"`, or `mfx:"-"` removes the field entirely.

## Relations

| Kind | Declared by | Relation key |
|---|---|---|
| BelongsTo (convention) | `UserID string` field → `User` | `user` |
| BelongsTo (explicit) | `ManagerID string \`mfx:"relation:Manager"\`` + `Manager User` companion | `manager` |
| HasMany | `Posts []Post` slice field | `posts` |
| ManyToMany | `Tags []Tag \`mfx:"through:ProductTag"\`` on both sides + junction model registered | `tags` |

Include in queries: `?include=user,posts,tags`. Nested filter: `?filter=user.role:eq:admin`.

Junction models for M2M are registered like any model.

## The 6-step pipeline

`Auth → Deserialize → Validate → Service → DB → Response`. Each step has a
default + `*StepRegistry` on `server.Pipeline`.

Middleware signature:
```go
type MiddlewareFunc func(ctx *maniflex.ServerContext, next func() error) error
```

Register:
```go
server.Pipeline.Service.Register(myFn,
    maniflex.ForModel("User", "Order"),                  // optional, by struct name
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),   // optional
    maniflex.AtPosition(maniflex.Before),                     // Before (default) | After | Replace
    maniflex.WithName("my-fn"),                           // optional, for traces
)
```

Operations: `OpList`, `OpRead`, `OpCreate`, `OpUpdate`, `OpDelete`, `OpOptions`, `OpAction`. A `HEAD` request runs as the `GET` it mirrors (`OpRead` / `OpList`); `OpHead` is never set.

`OpAction` uses a trimmed pipeline: `Auth → action middleware → handler → Response`.
Validate/Service/DB middleware never fires for actions.

**Short-circuit pattern (always pair these):**
```go
ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
return nil  // do NOT call next()
```

Calling `next()` after `Abort` lets downstream steps run and possibly overwrite `ctx.Response`. Always return without `next()`.

**Default step behaviours:**
- Auth: passthrough. Populate `ctx.Auth` here.
- Deserialize: parses query params → `ctx.Query`; body → `ctx.ParsedBody`; multipart → `ctx.Files`. 4 MB body limit.
- Validate: enforces `mfx:` tag rules on create/update. Strips `readonly`/`id`; strips `immutable` on update.
- Service: passthrough. Business logic goes here.
- DB: dispatches to adapter. Routes through `ctx.Tx` when set. Maps `ErrNotFound`→404, `*ErrConstraint`→409, `context.DeadlineExceeded`→504.
- Response: builds `APIResponse` from `ctx.DBResult`. List adds `meta`. Delete returns 204.

## ServerContext (per-request, not goroutine-safe)

Common fields:
- `Request *http.Request`, `Writer http.ResponseWriter`, `Ctx context.Context`
- `Model *ModelMeta`, `Operation Operation`, `ResourceID string`
- `RequestID string`, `TraceID string`
- `RawBody []byte`, `ParsedBody *RequestBody` (read-only — `ctx.Field` to read, `ctx.SetField`/`DeleteField` to mutate), `Query *QueryParams`, `Files map[string]*UploadedFile`
- `DBResult any` (`*ListResult` for list; the record otherwise — a typed `*T` on reads)
- `Response *APIResponse`
- `Auth *AuthInfo`, `Tx Tx`

Methods:
- `Abort(status int, code, message string)` — sets `ctx.Response`; caller returns nil without `next()`.
- `BindJSON(v any) error` — decode body into `v`; calls Abort on error.
- `URLParam(name) string`, `QueryParam(name) string`
- `Set(k, v)` / `Get(k) (any, bool)` — cross-step storage
- `Logger() *slog.Logger` — pre-seeded with request_id, trace_id, service
- `HasRole(role string) bool`
- `BeginTx(ctx, opts *TxOptions) (Tx, error)` — start a transaction
- `LockForUpdate(modelName, id) (map[string]any, error)` — requires `ctx.Tx`
- `RawQuery(sql, args...) ([]map[string]any, error)` — routes through ctx.Tx
- `RawExec(sql, args...) (int64, error)`
- `GetModel(name) *ModelAccessor` — CRUD on any registered model; routes through ctx.Tx

ModelAccessor methods: `List(q)`, `Read(id)`, `Create(data)`, `Update(id, data)`, `Delete(id)`.

`AuthInfo`:
```go
type AuthInfo struct {
    UserID       string
    Roles        []string
    Claims       map[string]any
    TenantID     string
    IdentityType AuthIdentityType  // IdentityHuman | IdentityServiceAccount | IdentityAnonymous
    Scopes       []string
    SessionID    string
    AuthMethod   string  // "jwt" | "api_key" | "session" | ...
}
```

## Transactions

```go
// Option A — middleware-wrapped, automatic commit/rollback
server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),  // nil opts = default isolation
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)

// Option B — manual
tx, err := ctx.BeginTx(ctx.Ctx, nil)
if err != nil { return err }
ctx.Tx = tx
defer tx.Rollback()    // no-op after Commit
// ... ctx.GetModel(...).Create/Update/Delete all routed through tx ...
return tx.Commit()
```

`WithTransaction` commits if `next()` returned nil AND `ctx.Response` is nil or 2xx. Otherwise rolls back. Idempotent — registering twice is fine, second call sees existing `ctx.Tx`.

SQLite does not support nested transactions. Use `_txlock=immediate` DSN for write-lock isolation.

## Errors

Sentinels (use `errors.Is` / `errors.As`):
```go
maniflex.ErrNotFound           // 404 NOT_FOUND
*maniflex.ErrConstraint        // 409 CONFLICT (Table, Column, Detail)
```

Built-in error codes the framework emits:
`INVALID_JSON`, `EMPTY_BODY`, `BODY_READ_ERROR`, `INVALID_QUERY`,
`MULTIPART_ERROR`, `NOT_FOUND`, `CONFLICT`, `VALIDATION_FAILED`,
`DATABASE_ERROR`, `TX_BEGIN_ERROR`, `TX_COMMIT_ERROR`, `NO_STORAGE`,
`TIMEOUT`, `PANIC`, `ENCRYPTION_NOT_CONFIGURED`.

Envelope: `{"error": {"code": "...", "message": "...", "details": ...}}`
Success envelope: `{"data": ...}`; list adds `"meta": {total, page, limit, pages}`.

## Querying (only on opt-in fields)

Operators: `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `like`, `ilike`, `in`, `not_in`, `is_null`, `not_null`.

```
?filter=status:eq:published
?filter=views:gte:100&filter=status:eq:published   # ANDed
?filter=tag:in:go,rust,zig
?filter=author.name:ilike:%ursula%                 # relation dot notation
?sort=created_at:desc&sort=title:asc
?include=user,comments,tags
?page=2&limit=20                                   # default 20, max 200
```

Soft-deleted rows are filtered out of list/read/include automatically. To see them: `?filter=deleted_at:not_null`.

## Custom Actions

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/{id}/cancel",
    Handler: cancelOrder,
    Middleware: []maniflex.MiddlewareFunc{
        auth.JWTAuth(secret),
        // any maniflex.MiddlewareFunc — runs between Auth and the handler
    },
})

func cancelOrder(ctx *maniflex.ServerContext) error {
    id := ctx.URLParam("id")
    var req MyReq
    if err := ctx.BindJSON(&req); err != nil { return nil }
    // ... work via ctx.GetModel / ctx.RawExec / ctx.BeginTx ...
    ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: result}
    return nil
}
```

Action handler owns body parsing, validation, transactions. Validate/Service/DB pipeline steps do NOT run for actions.

## File uploads

Tag a string field `mfx:"file,max_size:2MB,accept:image/*"`. Configure `maniflex.Config.FileStorage`.

Two upload styles:
1. Multipart POST/PATCH to the model endpoint — fields become `ctx.ParsedBody`, file parts become `ctx.Files`, storage key is written into the column.
2. Two-step: `POST /files` (multipart, field name `file`) returns `{"data":{"key":...}}`. Pass the key as a JSON string on the file field.

Standalone routes (when `FileStorage` set):
- `POST /files` — upload
- `GET /files/{key...}` — download (sets Content-Type, Content-Disposition, Content-Length)
- `DELETE /files/{key...}`

Without `FileStorage`, multipart and `/files/*` return 501 NO_STORAGE.

Auto-cleanup: stored file deleted on hard-delete or field overwrite. `auto_delete:false` opts out. Soft-delete does not trigger cleanup.

## Catalogue middleware

```go
import (
    "github.com/xaleel/maniflex/middleware/auth"
    "github.com/xaleel/maniflex/middleware/body"
    "github.com/xaleel/maniflex/middleware/validate"
    "github.com/xaleel/maniflex/middleware/service"
    "github.com/xaleel/maniflex/middleware/db"
    "github.com/xaleel/maniflex/middleware/response"
    "github.com/xaleel/maniflex/middleware/openapi"
    "github.com/xaleel/maniflex/middleware/idempotency"
)

// AUTH (Auth step)
auth.JWTAuth(secret, auth.JWTOptions{Issuer, Audience, TenantClaim, ScopesClaim, PublicKey})
auth.APIKeyAuth("X-API-Key", auth.APIKeyEntry{Key, Auth: maniflex.AuthInfo{...}}, ...)
auth.RequireRole("admin")
auth.AllowPublicRead()                       // passthrough on read/list; must run BEFORE an aborting authenticator
auth.BlockOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete)
// (no AllowPublicWrite — keep an op public by NOT scoping JWTAuth onto it; scoping is inclusion-only)

// BODY (Deserialize / Validate steps)
body.MaxBodySize(16 << 20)                   // override 4MB default
body.StripUnknownFields()
body.CoerceTypes()

// VALIDATE (Validate step)
validate.UniqueField(sqlDB, "email")
validate.RegexField("phone", `^\+?[0-9]{7,15}$`)
validate.ForbiddenValues("role", "superadmin")
validate.RequireAtLeastOne("name", "email")
validate.CrossFieldValidate(func(body map[string]any) error { return ... })
validate.DateRange("start_date", "end_date")              // end must not be before start
validate.RequireWhen("reason", "status:eq:rejected")      // conditional required

// SERVICE (Service step — usually Before)
service.HashField("password")                // bcrypt
service.SlugifyField("title", "slug")
service.SetField("user_id", func(ctx) any { return ctx.Auth.UserID })
service.StripField("password_confirm")
service.TimestampWhen("published_at", "status", "published")
service.OwnerScope("user_id")
// Side effects — register on DB step at AtPosition(After)
service.Emit(bus)
service.Webhook(service.WebhookConfig{URL, Secret})
service.SendEmail(mailer, func(ctx) *service.EmailMessage { return ... })

// DB (DB step)
db.ForceFilter("org_id", func(ctx) any { return ctx.Auth.Claims["org_id"] })
db.Tenancy("org_id", func(ctx) string { return ctx.Auth.TenantID })
db.Paginate(50)
db.RateLimit(db.RateLimitConfig{RequestsPerMinute: 10, Key: func(ctx) string {...}})
db.AuditLog(sink)                            // AtPosition(After), or default Before with db.WithChanges()
db.Invalidate(cache, func(ctx) []string { return ["keys", ...] })  // AtPosition(After)

// RESPONSE (Response step)
response.CORSHeaders("https://app.example.com")  // origins required; "*" allowed but panics with credentials
response.Cache(300)                          // AtPosition(After)
response.TransformField("avatar_url", func(v any) any { return cdn+v.(string) })
response.RedactField("phone", func(ctx) bool { return !ctx.HasRole("support") })
response.Envelope(func(ctx, data, meta) any { return ... })
response.AddHeader("Strict-Transport-Security", "max-age=63072000")
response.Logging(slog.Default())             // AtPosition(After)
response.Metrics(collector)                  // AtPosition(After)

// OPENAPI (OpenAPI.Generate step, maniflex.After position)
openapi.SetTitle("My API")
openapi.SetDescription("...")
openapi.AddServer("https://api.example.com", "Production")
openapi.AddSecurityScheme("bearerAuth", maniflex.OASSecurityScheme{Type: "http", Scheme: "bearer"})
openapi.AddExtension(func(spec *maniflex.OpenAPISpec) { /* mutate freely */ })

// IDEMPOTENCY (Deserialize step, AtPosition(After), scoped to OpCreate)
idempotency.Middleware(idempotency.Config{
    Store: maniflex.NewMemoryCache(),  // or any maniflex.CacheStore (e.g. Redis)
    TTL: 24 * time.Hour,
    KeyFunc: func(ctx) string { return ctx.Auth.UserID },
    HeaderRequired: false,
})
// Reads Idempotency-Key header. Replays cached 2xx for same key+body.
// Same key + different body → 422 IDEMPOTENCY_KEY_REUSED.
// Adds "Idempotent-Replayed: true" on replay.
```

## Encryption at rest

```go
import "github.com/xaleel/maniflex/pkg/encryption"

server := maniflex.New(maniflex.Config{
    KeyProvider: &encryption.EnvKeyProvider{Prefix: "MYAPP_KEY"},
    // or: &encryption.VaultKeyProvider{Address, Token, Mount: "transit"}
})

type Patient struct {
    maniflex.BaseModel
    SSN string `json:"ssn" mfx:"encrypted,key:patient-pii"`  // → MYAPP_KEY_PATIENT_PII env var
}
```

- Storage: `enc:<base64(envelope)>`.
- `EnvKeyProvider` needs base64-encoded 32-byte keys in env vars.
- Encrypted fields cannot be `filterable`/`sortable`.
- `encrypted,unique` adds `{field}_hmac TEXT UNIQUE` companion column.
- `maniflex.RotateEncryptionKey(ctx, server, "Model", oldKeyID, newKeyID)` re-encrypts in pages of 100. Keep both keys active.

## Versioning

```go
server.MustRegister(Invoice{}, maniflex.ModelConfig{Versioned: true})
// or VersionedDiffOnly: true to skip snapshots
```

Creates `invoice_histories` table with columns: `id, record_id, version, operation, actor_id, timestamp, request_id, diff, [snapshot]`.

Diff excludes hidden/writeonly/encrypted fields and HMAC companions. History table is read-only (writes return 405).

## Scheduled runner

```go
import "github.com/xaleel/maniflex/scheduled"

runner, _ := scheduled.New(server, scheduled.Config{
    Interval:  time.Minute,
    BatchSize: 500,
    OnDelete:   func(model, id string) { ... },
    OnSetField: func(model, id, field, to string) { ... },
})
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
runner.Start(ctx)
defer runner.Stop()
```

Scans `*time.Time` columns with `mfx:"scheduled"` tag. Per-model transactional batches. Hooks fire after commit. Use `runner.Sweep(ctx)` for one-shot ticks.

For multi-replica deployments: run runner in one replica, or use `maniflex/scheduled/jobsx` to dispatch sweeps through a job queue.

## Configuration

```go
type Config struct {
    Port            int            // default 8080
    PathPrefix      string         // default "/api"
    ServiceName     string         // adds "service" attr to logs, X-Service-Name header
    DB              DBAdapter      // required before Start
    DisableAutoMigrate bool        // migration runs by default; set to skip it
    QueryTimeout    time.Duration  // per-request DB deadline; 0 = unlimited
    ShutdownTimeout time.Duration  // default 30s
    Logger          *slog.Logger
    PanicLogger     *slog.Logger
    Trace           PipelineTrace  // {Enabled, Steps, Timings, Aborts, Bodies, Skips}
    FileStorage     FileStorage
    KeyProvider     KeyProvider
    HealthCheckDB   bool           // GET /health pings DB
    HealthTimeout   time.Duration  // default 3s
}
```

`maniflex.ConfigFromEnv()` reads PORT, PATH_PREFIX, DB_WRITE_URL, DB_READ_URL, SERVICE_NAME, LOG_LEVEL, etc.

## Database adapters

```go
// SQLite (dev)
db, err := sqlite.Open("./app.db", server.Registry())
db, err := sqlite.Open(":memory:", server.Registry())
db, err := sqlite.Open("file:./app.db?_txlock=immediate", server.Registry())

// PostgreSQL (prod) — Open(writeDSN, readDSN, registry) is positional.
db, err := postgres.Open(os.Getenv("DB_WRITE_URL"), os.Getenv("DB_READ_URL"), server.Registry())
// Pool/session tuning → OpenWithConfig(writeDSN, readDSN, registry, writePool, readPool, session):
db, err := postgres.OpenWithConfig(
    os.Getenv("DB_WRITE_URL"), os.Getenv("DB_READ_URL"), server.Registry(),
    postgres.PoolConfig{MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 30 * time.Minute}, // write
    postgres.PoolConfig{MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 30 * time.Minute}, // read
    postgres.SessionConfig{ApplicationName: "myapp"},
)
```

Reads route to ReadURL outside an active transaction; reads inside a tx go to write primary. AutoMigrate adds missing columns; never drops. Logs drift warnings.

## ModelConfig (per-model options)

```go
server.MustRegister(MyModel{}, maniflex.ModelConfig{
    TableName:         "custom_table",
    SoftDelete:        maniflex.SoftDeleteConfig{Enabled: true, Field: "deleted_at", FieldType: maniflex.SoftDeleteTimestamp},
    Middleware:        &maniflex.ModelMiddleware{
        Auth:        []maniflex.MiddlewareFunc{...},
        Deserialize: []maniflex.MiddlewareFunc{...},
        Validate:    []maniflex.MiddlewareFunc{...},
        Service:     []maniflex.MiddlewareFunc{...},
        DB:          []maniflex.MiddlewareFunc{...},
        Response:    []maniflex.MiddlewareFunc{...},
    },
    Versioned:         true,
    VersionedDiffOnly: false,
    Indices:           []maniflex.IndexSpec{{Name, Columns, Unique}},
})
```

`Register` accepts `...any`. Slice arguments are flattened one level — so you can do:
```go
var AuthModels = []any{User{}, Role{}}
var OrderModels = []any{Order{}, OrderLine{}}
server.MustRegister(AuthModels, OrderModels)  // both flattened
```

A `ModelConfig` value applies to the model immediately preceding it.

## Common patterns

### Public sign-up (scope auth away from it; inclusion-only)
```go
// Protect updates/deletes everywhere; protect creates only where needed.
// "User" is not scoped, so POST /users (sign-up) stays public.
server.Pipeline.Auth.Register(auth.JWTAuth(secret),
    maniflex.ForOperation(maniflex.OpUpdate, maniflex.OpDelete))
server.Pipeline.Auth.Register(auth.JWTAuth(secret),
    maniflex.ForModel("Post", "Comment"), maniflex.ForOperation(maniflex.OpCreate))
```

### Hash password on User create/update
```go
server.Pipeline.Service.Register(service.HashField("password"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
```

### Multi-tenancy
```go
server.Pipeline.DB.Register(db.Tenancy("org_id",
    func(ctx *maniflex.ServerContext) string { return ctx.Auth.TenantID }))
```

### Audit every write
```go
server.Pipeline.DB.Register(db.AuditLog(sink, db.WithChanges()),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))
// Note: WithChanges() requires default Before position, NOT After.
```

### Transactional create with stock lock
```go
server.Pipeline.Service.Register(maniflex.WithTransaction(nil),
    maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate))
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    bookID, _ := ctx.Field("book_id")
    book, err := ctx.LockForUpdate("Book", bookID.(string))
    if err != nil { return err }
    if book["stock"].(int64) < 1 {
        ctx.Abort(http.StatusConflict, "OUT_OF_STOCK", "")
        return nil
    }
    return next()
}, maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate))
```

### Custom default sort
```go
server.Pipeline.Deserialize.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if err := next(); err != nil { return err }
    if len(ctx.Query.Sorts) == 0 {
        ctx.Query.Sorts = append(ctx.Query.Sorts, maniflex.SortExpr{
            Field: "created_at", Direction: maniflex.SortDesc,
        })
    }
    return nil
}, maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpList), maniflex.AtPosition(maniflex.After))
```

### Background worker reading registered models
```go
// In a goroutine launched alongside server.Start(). Background code has no
// ServerContext — build one with NewBackground, then use ctx.GetModel.
bg := maniflex.NewBackground(context.Background(), server.DB(), server.Registry())
events := bg.GetModel("OutboxEvent")
rows, _ := events.List(&maniflex.QueryParams{
    Filters: []*maniflex.FilterExpr{{Field: "status", Operator: maniflex.OpEq, Value: "pending"}},
    Limit:   20,
})
for _, ev := range rows {
    // ... process ...
    events.Update(ev["id"].(string), map[string]any{"status": "done"})
}
```

## Project layout (recommended)

Small app — single `main.go`. Past ~5 models, split by responsibility:
```
main.go              wiring only (4 steps)
config.go            maniflex.Config assembly
models/              one file per model
middleware/          custom middleware + register.go
actions/             custom action handlers
internal/            framework-free business logic
```

Large monolith — split by domain:
```
domains/auth/       models.go + middleware.go + register.go (exports Models []any and Register(s))
domains/orders/     ditto
domains/catalog/    ditto
main.go             server.MustRegister(auth.Models, orders.Models, catalog.Models)
                    auth.Register(server); orders.Register(server); catalog.Register(server)
```

## Hard rules (ordered by frequency of being violated)

1. **Register models before `sqlite.Open` / `postgres.Open`.** Adapter reads
   registry at open time.
2. **After `ctx.Abort`, return without `next()`.** Abort only sets `ctx.Response`;
   it does not stop the chain.
3. **Soft-deleted rows are auto-filtered from list/read/include.** To see them,
   filter on the marker (`?filter=deleted_at:not_null`).
4. **`OpAction` skips Validate/Service/DB.** Per-action middleware list runs
   between Auth and the handler. The handler owns body parsing.
5. **`WithTransaction` on Service step requires `maniflex.Before` position** (default).
   Or use `maniflex.Replace` on the DB step.
6. **`db.AuditLog(sink, db.WithChanges())` must be registered at `maniflex.Before`
   position**, not After — it needs the pre-image.
7. **Encrypted fields can't be `filterable` or `sortable`.** For uniqueness use
   `encrypted,unique` (adds HMAC companion).
8. **`AutoMigrate` never drops columns.** Removes need explicit `ALTER TABLE`.
9. **Configure `FileStorage` before any model with `mfx:"file"` is exercised.**
   Without it, multipart uploads return 501.
10. **SQLite has no nested transactions.** Calling `BeginTx` inside an active
    SQLite tx fails. Reuse `ctx.Tx`.
11. **`Handler()` does not migrate.** Only `Start()` runs auto-migration. When
    mounting `Handler()` yourself, call `MigrateOnly`/`AutoMigrate` first.

## Testing pattern

```go
func newTestServer(t *testing.T) (*httptest.Server, *maniflex.Server) {
    t.Helper()
    server := maniflex.New(maniflex.Config{Port: 0, PathPrefix: "/api"})
    server.MustRegister(User{}, Post{})
    db, err := sqlite.Open(":memory:", server.Registry())
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { db.Close() })
    server.SetDB(db)
    // Handler() does not migrate — only Start() does. Migrate explicitly.
    if err := server.MigrateOnly(context.Background()); err != nil { t.Fatal(err) }
    middleware.Register(server)
    ts := httptest.NewServer(server.Handler())
    t.Cleanup(ts.Close)
    return ts, server
}
```

In-memory SQLite per test. `server.Handler()` returns the chi router but does
**not** migrate — `MigrateOnly` creates the tables up front.

## Sentinels & constants

```go
maniflex.OpCreate, maniflex.OpRead, maniflex.OpUpdate, maniflex.OpDelete, maniflex.OpList, maniflex.OpOptions, maniflex.OpAction
maniflex.Before, maniflex.After, maniflex.Replace
maniflex.OpEq, maniflex.OpNeq, maniflex.OpGt, maniflex.OpGte, maniflex.OpLt, maniflex.OpLte, maniflex.OpLike, maniflex.OpILike, maniflex.OpIn, maniflex.OpNotIn, maniflex.OpIsNull, maniflex.OpNotNull
maniflex.SortAsc, maniflex.SortDesc
maniflex.OnDeleteCascade, maniflex.OnDeleteSetNull, maniflex.OnDeleteRestrict, maniflex.OnDeleteNoAction
maniflex.IdentityHuman, maniflex.IdentityServiceAccount, maniflex.IdentityAnonymous
maniflex.SoftDeleteTimestamp, maniflex.SoftDeleteBool
maniflex.SchedSoftDelete, maniflex.SchedHardDelete, maniflex.SchedSetField
maniflex.ErrNotFound                  // sentinel error
*maniflex.ErrConstraint               // typed error with Table, Column, Detail
maniflex.ErrNoAdapter
maniflex.ErrFileNotFound
```

## What maniflex does NOT do

- No codegen. Reflection at registration only.
- No GraphQL.
- No built-in CLI. Use standard `go` tooling.
- No multi-database per server instance. One adapter per `*maniflex.Server`.
- No automatic column drops in AutoMigrate. Manual `ALTER TABLE` required.
- No SQLite nested transactions.
- No magic — every behaviour is a function in the `maniflex` package or a
  catalogue middleware. Read the source when in doubt.
````
