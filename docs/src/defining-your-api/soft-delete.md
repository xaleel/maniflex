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

The framework does not ship a built-in "undelete" endpoint, because the right
semantics differ across applications (does restoring also reset audit fields?
republish events?). The mechanics are simple: clear the marker. This is usually
done with a [custom action](../advanced-topics/actions.md) that runs a raw `UPDATE` or
calls the adapter directly.

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
