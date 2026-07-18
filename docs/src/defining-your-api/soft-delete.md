# Soft Delete

A soft-deleted row is left in the database but marked as deleted. `DELETE`
requests flip the marker; list, read, and include queries hide rows whose
marker is set. This page covers how a model opts in, the two storage styles,
and the query semantics that follow.

## Opting in

There are two ways to enable soft delete on a model. Both produce the same
behaviour; pick whichever fits the declaration style of the rest of the model.

### By embed

Embed one of the framework's marker types:

```go
type Article struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt              // timestamp-based soft delete
    Title string `json:"title"`
}
```

| Embed | Column added | Storage style |
|---|---|---|
| `maniflex.WithDeletedAt` | `deleted_at` — nullable timestamp; `NULL` means not deleted | timestamp |
| `maniflex.WithIsDeleted` | `is_deleted` — boolean; `false` means not deleted | flag |

Both columns are tagged `readonly` and `filterable`. They are not part of any
write request — the framework manages them.

### By configuration

The same setup, expressed at registration:

```go
server.MustRegister(
    Article{}, maniflex.ModelConfig{
        SoftDelete: maniflex.SoftDeleteConfig{
            Enabled:   true,
            Field:     "deleted_at",
            FieldType: maniflex.SoftDeleteTimestamp, // or maniflex.SoftDeleteBool
        },
    },
)
```

If both an embed and a `ModelConfig.SoftDelete` are present, the explicit
config wins.

## Choosing between timestamp and boolean

Both styles work; they differ in what you can tell from the column afterwards.

- **`WithDeletedAt`** records *when* the row was deleted, which makes audit
  trails, "deleted in the last 30 days" queries, and undelete-with-context
  possible. It is the default choice.
- **`WithIsDeleted`** stores only the fact of deletion. Use it when the
  surrounding system already records deletion timestamps elsewhere, or when a
  boolean fits an existing schema better.

## Delete semantics

For a soft-deletable model, `DELETE /api/<table>/{id}` updates the marker
instead of removing the row:

| Style | What `DELETE` does |
|---|---|
| Timestamp | sets `deleted_at` to the current UTC time |
| Boolean | sets `is_deleted` to `true` |

The endpoint, the response, and the status code are the same as for a
hard-delete model; only the underlying SQL differs.

## Query semantics

Once enabled, soft-deleted rows are filtered out everywhere the framework reads
the table:

- **List** (`GET /<table>`) — only un-deleted rows are returned.
- **Read** (`GET /<table>/{id}`) — a soft-deleted row returns `404`.
- **Includes** — relations populated via `?include=` skip soft-deleted children.
- **Update** — `PATCH` on a soft-deleted row returns `404`; the row is treated
  as absent.
- **Delete** — a second `DELETE` on the same row returns `404` and leaves the
  marker as it was, so the original deletion time survives. This holds inside a
  transaction too.

To surface the marker for clients that need it (e.g. an admin tool), filter on
it explicitly:

```bash
# Only soft-deleted rows
curl 'localhost:8080/api/articles?filter=deleted_at:ne:null'
```

`deleted_at` and `is_deleted` are `filterable`, so the standard filter grammar
applies — see [Querying](../using-the-api/querying.md).

## Restoring a row

Opt a model in with `ModelConfig.RestoreEnabled` to mount a restore endpoint:

```go
server.Register(Article{}, maniflex.ModelConfig{RestoreEnabled: true})
```

```bash
curl -X POST 'localhost:8080/api/articles/<id>/restore'
```

The request carries **no body** — it names the row in the URL and says only
"undo the delete". A successful restore returns `200` with the restored record,
so the client need not re-read it.

| Status | When |
|---|---|
| `200` | restored; the record is returned |
| `404` | no such row, **or** the row exists and is not deleted (mirroring the re-delete guard) |
| `501 RESTORE_UNSUPPORTED` | the configured adapter does not implement `Restorer` |

It is **off by default**, and deliberately so: un-deleting is a privileged
operation, and an endpoint that appeared merely because a version was upgraded
would not be covered by the authorisation an app had already written. The route
is only mounted for models that actually soft-delete — on a hard-delete model
there is nothing to restore.

### It dispatches as an update

A restore runs as `OpUpdate`, not as an operation of its own. That is the point:
every middleware an app already registered for "who may modify this row" — auth,
tenancy, force filters, audit — governs un-deleting it, with nothing to rewrite
and no new operation constant to discover.

```go
// This already covers restore. Nothing further to do.
server.Pipeline.Auth.Register(auth.RequireRole("editor"),
    maniflex.ForOperation(maniflex.OpUpdate))
```

Where the two must be told apart — an audit sink recording "restored" rather
than "updated", or a validation rule that only makes sense against a body — read
`ctx.IsRestore()`. Note a restore's `ctx.ParsedBody` is empty, so body-driven
validation middleware sees nothing to check.

Scoping is enforced too. Because a soft-deleted row is invisible to every read
path, the restore cannot read its target back to check scope the way an update
does; instead the request's forced filters are applied to the restore statement
itself. A caller cannot un-delete a row outside their tenancy by knowing its id.

### What it writes

Only the delete marker is cleared. `updated_at` is left untouched, so a restore
does not read as an edit to caches, sync clients, or anything else watching that
column — the audit or event record is where a restore is recorded.

**Cascade is not undone.** A restore brings back the row it names and nothing
else: nothing records which children an `onDelete:cascade` removed, so restore
each explicitly, or model the relationship so the children survive the parent's
deletion. See [Relations](relations.md#cascading-deletes).

### Custom adapters

The endpoint needs a database adapter implementing `maniflex.Restorer`. The
bundled SQLite and Postgres adapters do; a third-party adapter that does not is
unaffected and answers `501`. It is a separate interface, not a `DBAdapter`
method, so adding it broke nothing:

```go
type Restorer interface {
    Restore(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (any, error)
}
```

Apply `q`'s filters (the request's forced scope, possibly nil) to the statement,
and return `ErrNotFound` when no row matches — including a row that exists but is
not deleted.

## Interaction with hard delete

A model is either soft- or hard-delete; the choice is a property of the model,
not the request. If you need a true hard delete on a soft-deletable model — for
example, to honour an erasure request — perform it through a raw query or a
custom action that bypasses the standard handler.

## Quick reference

| Goal | Declaration |
|---|---|
| Timestamp soft delete | embed `maniflex.WithDeletedAt` |
| Boolean soft delete | embed `maniflex.WithIsDeleted` |
| Soft delete with a custom column | `ModelConfig.SoftDelete` |
| List only deleted rows | `?filter=deleted_at:ne:null` (timestamp) or `?filter=is_deleted:eq:true` (boolean) |
| Restore a deleted row | `ModelConfig.RestoreEnabled` → `POST /:model/{id}/restore` |
