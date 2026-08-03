# FAQ & Troubleshooting

Common questions and the pitfalls that catch new users. Each entry links to
the page that covers the underlying concept in depth.

## Registration & startup

### "I registered a model after `SetDB` and nothing happens."

The adapter is built from the registry; once it's open, later
registrations don't reach it. Register every model before opening the
database:

```go
server.MustRegister(User{}, Order{}, Invoice{})   // 1. registry populated
db, _ := sqlite.Open("./app.db", server.Registry()) // 2. adapter built
server.SetDB(db)                                    // 3. adapter wired in
```

See [Architecture](../how-it-works/architecture.md) and [Models & BaseModel](../defining-your-api/models.md).

### "`AutoMigrate` didn't add my new column."

`AutoMigrate` adds missing columns but never drops or alters existing ones.
If your struct says `string` and the table column is `INTEGER`, the
migrator leaves it alone and logs a drift warning. Inspect the warning
log, then either fix the struct or run a manual `ALTER TABLE`.

### "Why does the framework panic when a struct doesn't embed `BaseModel`?"

Because every model needs an `id` and timestamp columns, and `BaseModel`
provides them. The check is deliberate; embedding `BaseModel` is one
line. See [Models & BaseModel](../defining-your-api/models.md).

### "Can I rename the `id` column?"

No. The framework hard-codes `id` as the primary-key column name across
the adapter, the relation resolver, and the OpenAPI generator. Pick a
table prefix or rename the table instead.

### "Can my records use integer ids, or ids the client supplies?"

No to both. Identity is one string column holding a framework-generated
UUIDv4, and no route accepts an id from a request — an `"id"` in a write
body is ignored rather than rejected. Server-side code writing through
`ctx.GetModel(...).Create` *may* assign its own id, and then owns
uniqueness and URL-safety. Composite keys are not representable at all.
The full contract, including what a natural key should look like instead,
is [Record Identity](../defining-your-api/identity.md).

## Pipeline & middleware

### "My middleware doesn't fire — I scoped it to `OpAction`."

`OpAction` requests run a trimmed pipeline: `Auth → action middleware →
handler → Response`. Middleware registered on Validate, Service, or DB
with `ForOperation(OpAction)` is never reached. Move per-action logic
into the action's own middleware list:

```go
server.Action(maniflex.ActionConfig{
    Method:     "POST",
    Path:       "/orders/place",
    Handler:    placeOrder,
    Middleware: []maniflex.MiddlewareFunc{auth.JWTAuth(secret), checkStock},
})
```

See [Custom Endpoints](../advanced-topics/actions.md).

### "`ctx.Abort` was called but the database still got written to."

Probably called `next()` after `Abort`. `Abort` only populates
`ctx.Response`; it does not stop the chain. Return without `next()` and
the chain unwinds. See the "Calling `next()` after Abort" section of
[ServerContext](../the-request-pipeline/context.md).

### "Why doesn't `After`-position middleware see my error?"

It does — through `ctx.Response`:

```go
func afterDB(ctx *maniflex.ServerContext, next func() error) error {
    if err := next(); err != nil { return err }
    if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
        return nil // skip the side effect
    }
    record(ctx)
    return nil
}
```

A non-nil error from `next()` and a 4xx/5xx `ctx.Response` are distinct
signals — both mean "the default step refused to succeed."

### "I want the same middleware on every model except one."

Register it without `ForModel`, then register a passthrough that
short-circuits for the excluded model — or guard inside the middleware:

```go
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if ctx.Model.Name == "PublicResource" {
        return next()
    }
    // ... usual work ...
    return next()
})
```

## Transactions

### "`LockForUpdate` returned an error: 'requires an active transaction'."

Pessimistic locks only make sense inside a transaction. Wrap the call:

```go
tx, err := ctx.BeginTx(ctx.Ctx, nil)
if err != nil { return err }
ctx.Tx = tx
defer tx.Rollback()

row, err := ctx.LockForUpdate("StockBalance", id)
// ...

return tx.Commit()
```

…or register `maniflex.WithTransaction(nil)` on the Service step so the
transaction is already open by the time the lock fires.

### "`WithTransaction` registered twice on SQLite — got a 'tx already active' error."

SQLite does not support nested transactions. `WithTransaction` is
idempotent — registering it twice is fine — but calling `ctx.BeginTx`
manually inside an already-open SQLite transaction will fail. Reuse
`ctx.Tx` instead of starting a new one.

### "My transaction committed even though I returned an error."

Three things to check:

- The error was returned from `next()`, not swallowed inside the
  middleware.
- The middleware did not call `next()` *and* return its own error — the
  framework treats those independently.
- `ctx.Response.StatusCode` is `>= 400` *or* `next()` returned non-nil.
  Both abort the commit.

## Querying

### "`?filter=foo:eq:bar` returns `400 INVALID_QUERY`."

The field isn't `filterable`. Add the tag:

```go
Foo string `json:"foo" mfx:"filterable"`
```

The check is intentional — exposing every column to client filters lets
clients build arbitrary indexes against you. Opt in per field.

### "Nested filter `?filter=author.name:eq:X` doesn't work."

Two conditions:

- The relation must be declared on the current model (`AuthorID` for
  convention, `relation:Author` for explicit).
- The target field must itself be `filterable` on the related model.

See [Relations](../defining-your-api/relations.md) and [Querying](../using-the-api/querying.md).

### "I want a default sort order."

There isn't a built-in "default sort" tag. Register a Deserialize
middleware that appends to `ctx.Query.Sorts` when the client doesn't
supply one:

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

### "Soft-deleted rows show up in my admin tool list."

They don't — the framework filters them out everywhere. To *see* them,
filter on the marker explicitly:

```bash
curl '...?filter=deleted_at:not_null'
```

See [Soft Delete](../defining-your-api/soft-delete.md).

## Files & encryption

### "My multipart upload returns 501 NO_STORAGE."

`Config.FilesConfig.Storage` is `nil`. Configure a backend:

```go
fs, _ := storage.NewLocalStorage("./uploads")
server := maniflex.New(maniflex.Config{
    FileStorage: fs,
    // other application settings
})
```

See [File Fields & Uploads](../defining-your-api/files.md).

### "Encrypted field rejected my write: ENCRYPTION_NOT_CONFIGURED."

Same shape — `Config.KeyProvider` is `nil`. Configure one (e.g.
`encryption.EnvKeyProvider`) before any model with `mfx:"encrypted"` is
exercised. See [Encryption at Rest](../advanced-topics/encryption.md).

### "I added `mfx:"unique"` to an encrypted field and got a duplicate-key error on legit data."

The framework stores a keyed HMAC of the plaintext in a `{field}_hmac`
companion column. Two plaintexts that hash to the same digest is a
collision — astronomically unlikely with SHA-based HMAC. More likely you
have legacy rows whose digests were made with a field-encryption key. Configure
the provider's stable `IndexKeyID`, quiesce writes, and backfill those companion
digests before online encryption-key rotation. `RotateEncryptionKeyWithOptions`
preflights the digests and reports every mismatched row instead of changing
them during rotation.

## Auth

### "I added `auth.JWTAuth` and sign-up stopped working."

Sign-up is a write — a global `auth.JWTAuth` rejects it because no token
exists yet. There is no `AllowPublicWrite` helper; instead **scope the
authenticator** so it never runs on the sign-up path. Auth scoping
(`ForModel` / `ForOperation`) is *inclusion-only*: a middleware runs only
for the models and operations you scope it to, so anything you leave out of
every auth registration stays public.

```go
// Protect updates and deletes everywhere…
server.Pipeline.Auth.Register(
    auth.JWTAuth(secret),
    maniflex.ForOperation(maniflex.OpUpdate, maniflex.OpDelete),
)
// …and protect creates only on the models that need a session. "User" is
// deliberately not listed, so POST /users (sign-up) is covered by no auth
// registration and stays public.
server.Pipeline.Auth.Register(
    auth.JWTAuth(secret),
    maniflex.ForModel("Post", "Comment"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

Scoping the authenticator this way is safe only while the model list is. Because
the filters are inclusion-only, a model registered later is named in no
registration and so is covered by no auth at all — silently. Prefer
`auth.AllowAnonymous` for anything but the smallest API: it leaves `JWTAuth`
registered globally and makes the *exemption* the thing you enumerate, so an
unlisted route refuses rather than opens.

```go
server.Pipeline.Auth.Register(auth.AllowAnonymous(),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
)
server.Pipeline.Auth.Register(auth.JWTAuth(secret))
```

It is also the only way to have a route that authenticates a token when one is
sent and still serves a caller who sends none — a `JWTAuth` scoped away from the
route does not run there, so even a valid token leaves `ctx.Auth` nil.
`auth.AllowPublicRead` is a passthrough, not an exemption: an aborting
authenticator answers 401 before it is reached, so it needs `AllowAnonymous` in
front of the authenticator to do anything at all.

### "JWT keeps returning 401 — the token validates manually."

Three usual suspects:

- Algorithm mismatch — `HS256` token verified against an `RS256` public
  key (or vice versa). Set `JWTOptions.PublicKey` for asymmetric keys.
- `Issuer` / `Audience` mismatch — the framework rejects tokens whose
  `iss` / `aud` doesn't match the configured value.
- Clock skew — the token's `nbf` is in the future or `exp` is in the
  past on the server clock. Sync NTP.

## Database

### "Why does `Tenancy` middleware not apply to my reads?"

It does, including reads — `Tenancy` filters every operation, including
list and read. If you're seeing rows from another tenant, check that the
middleware is registered without a `ForOperation` filter and that
`ctx.Auth.TenantID` is being populated by the upstream Auth middleware.

### "My Postgres reads have stale data after a write."

Read replicas have replication lag. The framework routes reads to the
replica only outside an active transaction; reads inside a
`WithTransaction`-managed request go to the primary. For
read-your-writes outside a transaction, run the query through
`ctx.RawQuery` against an explicit primary connection, or shorten the
client's expected window.

### "I dropped a column from my struct — `AutoMigrate` didn't remove it."

By design. The migrator never drops; it logs a drift warning so you see
the column still exists. Remove it with an explicit `ALTER TABLE … DROP COLUMN`
during a maintenance window.

### "I changed a field's type — the column still has the old one."

Also by design. `AutoMigrate` adds columns; it never rewrites one, because
converting a column's type can lose data and locks the table while it runs. It
logs a drift warning naming the table, the column, the type the database has and
the type the model wants, at every startup. Convert the column with an explicit
versioned migration (`ALTER TABLE … ALTER COLUMN`, plus whatever backfill the
new type needs).

### "`504 TIMEOUT` on a query that used to work."

`Config.QueryTimeout` fired. The deadline is per request, applied to
`ctx.Ctx`. Either:

- Raise the timeout (`30s` is a common ceiling).
- Speed up the query (missing index, expensive include, large LIMIT).
- Add a `db.Paginate` cap on the offending list endpoint.

A `504` always means a server-side deadline expired. A client that gives up and
disconnects mid-request is logged as `499` instead, so the two do not share a
line in your metrics.

## OpenAPI

### "`/openapi.json` is empty."

You didn't register any models before calling `Handler` or `Start`. The
generator reads the registry at request time, but that registry is sealed when
the router is built; late model registration is rejected so the specification
and mounted routes cannot disagree.

### "I customised the spec but my changes don't appear."

Register the customisation at `maniflex.After` position on the Generate step,
not Before. Before runs against an empty spec; After mutates the
just-generated document.

```go
server.Pipeline.OpenAPI.Generate.Register(
    openapi.SetTitle("My API"),
    maniflex.After, // <-- not the default Before
)
```

## Production

### "My pod terminated mid-request on deploy."

Kubernetes sent `SIGTERM`, gave you `terminationGracePeriodSeconds`,
then `SIGKILL`'d the process. `Config.ShutdownTimeout` defaults to 30s;
set the pod's grace period larger (60s is comfortable) so the graceful
path has time to complete. See [Graceful Shutdown](../deployment/shutdown.md).

### "Readiness returns 503 intermittently."

`/ready` pings the database and runs every `Config.ReadinessChecks` entry, so
`503` means a dependency did not answer in time. Tune:

- `Config.HealthTimeout` — should be shorter than the probe's
  `timeoutSeconds`.
- The database — if the pool is exhausted, `db.Ping()` waits for a
  connection.
- Your own checks — they run on every probe request, so a full round-trip
  through a dependency will eventually time one out.

The same applies to `/health` when `Config.HealthCheckDB` is true.

Which dependency failed is in the log, not the response — the body reports only
`{"status":"not_ready"}` unless `Config.Probes.PublishReadinessChecks` is set,
so that a public probe does not name what you depend on.

### "Kubernetes restarts my pods whenever the database blips."

The liveness probe is pointed at a database-backed endpoint — `/api/ready`, or
`/api/health` with `HealthCheckDB` on. Point it at `/api/live`, which answers
`200` for as long as the process is alive and never touches a dependency.
Restarting a replica cannot fix a database, and doing it to every replica at
once is how an outage gets worse. See
[Configuration](../deployment/config.md#probes).

### "My auth middleware doesn't run on `/health` or `/ready`."

By design. The probes are mounted straight onto the router and never enter the
model pipeline, so no `Pipeline.Auth` middleware sees them and
`authx.AllowPublic` has nothing to exempt them from — an orchestrator's probe is
the canonical unauthenticated request.

Use `Config.Probes` when that is not what you want. It gates or unmounts each
probe independently, and it is the only lever scoped to the probes alone
(`Config.HTTPMiddlewares` reaches them, but reaches every other route too):

```go
cfg.Probes = maniflex.ProbesConfig{
    Ready:  maniflex.ProbeConfig{Middleware: []maniflex.HTTPMiddleware{probeToken}},
    Health: maniflex.ProbeConfig{Disabled: true},
}
```

Gate `/ready`, not `/live`. A liveness probe that gets a `401` is a liveness
probe that fails, and the answer to that is a `SIGKILL` mid-drain. See
[Gating and unmounting the probes](../deployment/config.md#gating-and-unmounting-the-probes).

### "Logs are noisy with debug records."

`Config.Trace.Enabled = true` was left on, or the `Logger` accepts DEBUG.
Set the handler's level to INFO in production. Trace flags are opt-in
specifically because they're high-volume.

## Library design questions

### "Why reflection instead of codegen?"

To avoid the regeneration step. A `mfx:` tag change is in effect on the
next process start; nothing to rebuild. The cost is one reflection pass
per model at boot — usually under a millisecond per model. See
[Architecture](../how-it-works/architecture.md).

### "Can I use maniflex with GraphQL?"

The generated routes are REST. You can put GraphQL in front (`gqlgen`,
`graphql-go`) and have resolvers call `ctx.GetModel(...).List` / `.Read`
/ etc. — the model accessor doesn't care which HTTP layer is on top.

### "Can I use multiple databases?"

One adapter per `*maniflex.Server`. For multi-database setups, run two servers
(possibly in the same process) and route between them at the application
layer, or use raw queries from custom actions that target the secondary
database directly.

### "Is there a CLI?"

Not yet. The framework is intentionally library-only — no
project-scaffolding command, no migration runner. Use the standard Go
toolchain and an external migration tool. The
[App Anatomy](../anatomy.md) page describes the recommended project layout.

## Where to find more

If your question isn't here, three places to look next:

- The page for the concept involved — every link in this FAQ points to it.
- The framework's own e2e tests under `tests/e2e/` — they exercise every
  edge case the docs describe.
- The source. The `maniflex` package is small; reading the implementation of
  a step or a middleware is often faster than guessing.
