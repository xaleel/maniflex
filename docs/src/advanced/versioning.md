# Versioning & History

A model marked `Versioned` keeps an immutable history of every write to it.
The framework creates a sibling `{model}_history` table at migration time
and appends one row per `Create` / `Update` / `Delete`. History rows are
queryable through the same REST surface as any other model, with one
restriction: they are read-only.

## Opting in

Set `Versioned: true` in `ModelConfig`:

```go
server.MustRegister(
    Invoice{}, maniflex.ModelConfig{
        Versioned: true,
    },
)
```

Equivalent declaration on the embedded `BaseModel`:

```go
type Invoice struct {
    maniflex.BaseModel `mfx:"versioned"`
    Number string  `json:"number" mfx:"required,unique"`
    Amount float64 `json:"amount" mfx:"required,min:0"`
}
```

Either form triggers two effects at registration:

1. A synthetic `InvoiceHistory` model is added to the registry — same as
   any other model, but read-only.
2. Three DB middlewares are attached to `Invoice`: a pre-image capture
   before `OpUpdate` / `OpDelete`, and an After-DB writer for every
   write that succeeded.

## The history table

The sibling table has a fixed schema, regardless of the source model's
columns:

| Column | Type | Notes |
|---|---|---|
| `id` | TEXT | UUID, primary key of the history row itself |
| `record_id` | TEXT | id of the source row this entry describes |
| `version` | INTEGER | 1-based, monotonic per `record_id` |
| `operation` | TEXT | `"create"`, `"update"`, or `"delete"` |
| `actor_id` | TEXT | `ctx.Auth.UserID` at the time of the write; nullable |
| `timestamp` | TIMESTAMP | UTC, set by the framework |
| `request_id` | TEXT | the `X-Request-Id` of the producing request |
| `diff` | TEXT | JSON `{field: {old, new}}` map |
| `snapshot` | TEXT | full row state as JSON — omitted when `VersionedDiffOnly` is set |

`AutoMigrate` also adds an index `idx_{table}_history_record_version` on
`(record_id, version DESC)` for the standard "list history for one row"
query.

## What gets diffed

`diff` records every changed scalar field. The format is:

```json
{
  "amount":   {"old": 99.0, "new": 105.0},
  "status":   {"old": "draft", "new": "sent"}
}
```

- **`OpCreate`** — every field is recorded as `{"old": null, "new": value}`.
- **`OpUpdate`** — only fields whose value differs between pre-image and
  post-image appear.
- **`OpDelete`** — every field is recorded as `{"old": value, "new": null}`.

Excluded by default:

- The primary key (`id`).
- `hidden` fields.
- `writeonly` fields.
- `encrypted` fields and their `{field}_hmac` companions.

This avoids leaking secrets into history while still capturing the
business-meaningful changes.

## Snapshot vs. diff-only

By default each history row carries both the `diff` and the full
`snapshot` of the row state — convenient for "what did the record look
like on date X?" queries:

```bash
curl 'localhost:8080/api/invoice_histories?filter=record_id:eq:abc123&sort=version:desc&limit=1'
```

For high-write models the snapshot is the largest column by far.
`VersionedDiffOnly: true` skips the snapshot entirely:

```go
server.MustRegister(
    EventLog{}, maniflex.ModelConfig{
        Versioned:          true,
        VersionedDiffOnly:  true,
    },
)
```

The trade-off: reconstructing the row state at version N requires
walking all entries from version 1 to N and applying their diffs. For an
audit trail used by humans (reading recent changes) this is fine; for
point-in-time recovery, keep the snapshot.

## Reading history

The history model is a normal registered model. The standard list and
read endpoints work:

```bash
# All history rows for one invoice, newest first.
curl 'localhost:8080/api/invoice_histories
     ?filter=record_id:eq:abc123
     &sort=version:desc'

# Recent activity by an actor.
curl 'localhost:8080/api/invoice_histories
     ?filter=actor_id:eq:user-alice
     &sort=timestamp:desc
     &limit=50'
```

`record_id`, `operation`, `actor_id`, and `request_id` are filterable;
`version` and `timestamp` are sortable. Write operations (`POST`,
`PATCH`, `DELETE`) on the history endpoint return `405 METHOD_NOT_ALLOWED`
— the history is append-only by construction.

The history rows participate in OpenAPI generation, so `/openapi.json`
documents the endpoint alongside everything else.

## Transactions and history

The history row is written in the **same transaction** as the source
write — both succeed together or neither does. If the primary insert
rolls back, no orphan history entry is left behind.

If the history write itself fails after a successful primary write, the
framework logs the error but does **not** fail the primary response.
Losing one history row is preferable to refusing a write that the user
already saw succeed. The error is logged via `ctx.Logger()` so an
operator can investigate.

## Performance notes

- One additional `INSERT` per write to a versioned model. Postgres handles
  this with a write multiplier of ~2x on the affected tables.
- The `snapshot` JSON is the dominant cost on row size. Use
  `VersionedDiffOnly` for verbose tables.
- The `record_id` index is essential — every "history for one row" query
  uses it. Don't drop it.
- For very-high-write models, consider routing history to a separate
  table partition or a write-optimised store (TimescaleDB,
  ClickHouse) via a custom DB-After middleware instead of the built-in.

## Comparison with audit logging

[Audit Logging](audit.md) and Versioning solve different problems:

| | Versioning | Audit Logging |
|---|---|---|
| Storage | sibling DB table | configurable sink (DB, syslog, SIEM, …) |
| Granularity | per-row | per-row, optionally with diff |
| Transactional with the write | yes | yes (Before-DB) |
| Reconstruct prior state | yes — via snapshot or diff replay | no — only the change is recorded |
| Read API | the framework's list/read on `{model}_history` | up to the sink |
| Best for | "what did this invoice look like a week ago?" | "who did what, when, across the whole system?" |

The two compose cleanly — turn on versioning for models that need
reconstructable history, and audit-log everything for compliance.

## Operational checklist

- Enable `Versioned` on models whose change history matters for
  compliance, debugging, or undo. Don't enable it on every model — the
  write multiplier adds up.
- Choose `VersionedDiffOnly: true` for high-write tables where the
  diff alone is enough.
- Plan storage growth: history is monotonic — older rows never go away
  unless you delete them out of band. Set up a retention job for very
  active models.
- Restrict access to the history endpoints with `auth.RequireRole` —
  the diff and snapshot may contain values an end user shouldn't see.
