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

`AutoMigrate` also adds a **unique** index `uidx_{table}_history_record_version`
on `(record_id, version DESC)` for the standard "list history for one row"
query. The uniqueness guards against two concurrent writes computing the same
`(record_id, version)`; the writer retries on the constraint violation.

## What gets diffed

`diff` records every changed scalar field. The format is:

```json
{
  "amount":   {"old": 99.0, "new": 105.0},
  "status":   {"old": "draft", "new": "sent"}
}
```

- **`OpCreate`** — every field with a non-nil value is recorded as
  `{"old": null, "new": value}`; nil-valued fields are skipped.
- **`OpUpdate`** — only fields whose value differs between pre-image and
  post-image appear.
- **`OpDelete`** — every field is recorded as `{"old": value, "new": null}`.

Excluded by default:

- The primary key (`id`) — from the diff only; the snapshot keeps it.
- `hidden` fields.
- `writeonly` fields.
- `encrypted` fields and their `{field}_hmac` companions.

This avoids leaking secrets into history while still capturing the
business-meaningful changes. **The same exclusions apply to the
`snapshot`** — history rows are built from the decrypted row, so an
encrypted column left in the snapshot would sit in the history table as
plaintext and quietly undo the at-rest guarantee. If you need a value
in history, don't mark it `encrypted`, `hidden` or `writeonly`.

## Snapshot vs. diff-only

By default each history row carries both the `diff` and the full
`snapshot` of the row state — convenient for "what did the record look
like on date X?" queries:

```bash
# The newest history row for one invoice.
curl 'localhost:8080/api/invoices/abc123/history?limit=1'
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

History is read **through the record it belongs to**:

```bash
# One invoice's history, newest first.
curl 'localhost:8080/api/invoices/abc123/history'

# Paginated.
curl 'localhost:8080/api/invoices/abc123/history?page=2&limit=50'
```

The response is the standard list envelope. Rows come back newest-first
(descending `version`); `page` and `limit` are honoured, and the default
page size is 20.

### Why not a flat `/invoice_history` endpoint?

There used to be one, and it was a security hole (audit MS-4). The
synthesized history model mounted the full read surface, but per-model
middleware is registered against the **parent's** name — an app that
wrote:

```go
server.MustRegister(Invoice{}, maniflex.ModelConfig{
    Versioned:  true,
    Middleware: &maniflex.ModelMiddleware{Auth: []maniflex.MiddlewareFunc{requireLogin}},
})
```

protected `GET /invoices` and left `GET /invoice_history` open to
anyone. `db.Tenancy("org_id", …)` scoped with `ForModel("Invoice")` had
the same gap, so any caller could read every tenant's history.

Copying the parent's middleware onto the history model does not fix it.
The history table has none of the parent's columns — it holds `id`,
`record_id`, `version`, `operation`, `actor_id`, `timestamp`,
`request_id`, `diff` and `snapshot` — so a tenancy filter on `org_id`
has nothing to filter. It would look scoped and enforce nothing.

So the history model is now `Headless` (registered and migrated, no
routes of its own) and reached only through the parent. The request runs
the parent's read pipeline first: **if you cannot read the record, you
cannot read its history**, and you get the same `404` the record itself
would give you rather than a `403` that confirms it exists. Every auth,
tenancy and force-filter middleware you already registered applies, with
nothing new to configure.

Middleware scoped with `ForOperation(OpRead)` **does** match these requests,
and that is what makes the gate work: it reads the request's forced filters, so
a tenancy middleware that never ran would leave it with nothing to scope by.
The implication runs one way only — `ForOperation(maniflex.OpReadHistory)`
means history requests alone.

### Deleted records

A **soft-deleted** record keeps its history, including the delete entry.
The gate uses the adapter's `ScopeChecker` capability, which counts
soft-deleted rows as present while still applying your scope — so a
deleted record's history is visible to exactly the callers who could see
the record, and to nobody else.

A **hard-deleted** record's history is not reachable over HTTP. The row
that said who was allowed to read it is gone, and answering from the
history table alone would mean showing it either to everyone or to no
one. The rows remain in `{model}_history` for an admin query or an
offline audit. If you need history to outlive deletion, use soft-delete
(`maniflex.WithDeletedAt`).

#### Custom adapters

`ScopeChecker` is optional. An adapter that does not implement it — a
third-party one written against the `DBAdapter` interface — keeps
working, and the endpoint stays **exactly as scoped**: the gate falls
back to an ordinary scoped read, so tenancy and force filters apply as
they always did. The one thing it gives up is the soft-delete case: that
read applies the soft-delete condition, so a soft-deleted record's
history 404s, the same as a hard-deleted one.

There is no warning at startup, because nothing is misconfigured — you
did not ask for a capability and fail to get it. To gain it, implement:

```go
func (a *MyAdapter) ExistsInScope(
    ctx context.Context, model *maniflex.ModelMeta, id string,
    filters []*maniflex.FilterExpr,
) (bool, error)
```

Report whether a row with that id exists and satisfies `filters`,
**including when it is soft-deleted**. Apply the filters in full —
"deleted" must not become "unscoped" — and return only the boolean, never
the row: a method that returned soft-deleted records would be a general
bypass of the soft-delete condition, and this one is deliberately unable
to serve as one. Implementing it on your `Tx` type as well lets a
history read inside a transaction see the request's own uncommitted
writes.

### Filtering

Query parameters are parsed against the **parent** model, so
`?filter=operation:eq:update` is rejected — `operation` is not a field
of `Invoice`. Filtering within a record's history is not currently
supported; a record's history is bounded by that record's edit count,
and pagination covers it. For cross-record queries ("everything alice
changed this week"), use [audit logging](audit.md), which is built for
exactly that question.

The history model is `Headless`, so it contributes a schema to
`/openapi.json` but no paths; the `/{id}/history` route is documented on
the parent.

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
