# Audit Logging

`db.AuditLog` from the catalogue records every mutating operation to a
configured sink. Unlike [Versioning](versioning.md) — which writes to a
sibling table in the same database — audit log records are designed to be
shipped to an external system (a database table, a structured logger, a
SIEM). This page documents the record shape, the sink contract, and the
options that change what is captured.

## Registering

The simplest registration captures the operation without per-field diffs:

```go
import "maniflex/middleware/db"

server.Pipeline.DB.Register(
    db.AuditLog(mySink),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)
```

`mySink` implements `db.AuditSink`:

```go
type AuditSink interface {
    Write(ctx context.Context, record AuditRecord) error
}
```

The audit record is emitted from a background goroutine with a 5-second
timeout. Sink errors are logged but never fail the request — audit
writes are fire-and-forget, by design. An audit pipeline that can fail
the request is a liveness risk; an audit pipeline that occasionally
drops a record is recoverable.

## The record shape

Every audited write produces one `AuditRecord`:

```go
type AuditRecord struct {
    Timestamp   time.Time              `json:"timestamp"`
    Model       string                 `json:"model"`
    Operation   maniflex.Operation          `json:"operation"`
    ResourceID  string                 `json:"resource_id,omitempty"`
    Actor       string                 `json:"actor,omitempty"`
    TenantID    string                 `json:"tenant_id,omitempty"`
    RequestID   string                 `json:"request_id,omitempty"`
    TraceID     string                 `json:"trace_id,omitempty"`
    ServiceName string                 `json:"service_name,omitempty"`
    Result      any                    `json:"result,omitempty"`
    Changes     map[string]FieldChange `json:"changes,omitempty"`
}

type FieldChange struct {
    From any `json:"from"`
    To   any `json:"to"`
}
```

| Field | Source |
|---|---|
| `Timestamp` | UTC at the moment the record is built |
| `Model` | `ctx.Model.Name` |
| `Operation` | `ctx.Operation` |
| `ResourceID` | `ctx.ResourceID` — empty on create until after the write |
| `Actor` | `ctx.Auth.UserID` (empty for anonymous requests) |
| `TenantID` | `ctx.Auth.TenantID` |
| `RequestID` | `ctx.RequestID` (chi's `X-Request-Id`) |
| `TraceID` | `ctx.TraceID` (W3C `traceparent`) |
| `ServiceName` | `Config.ServiceName` |
| `Result` | `ctx.DBResult` — the row state returned by the adapter |
| `Changes` | populated only when `WithChanges()` is set |

The minimum shape — `Timestamp`, `Model`, `Operation`, `Actor`,
`RequestID` — is enough to answer "who did what, when?" for compliance.
Adding `Changes` answers "what specifically was modified?"

## Tracking changes

`WithChanges()` enables per-field diffs:

```go
server.Pipeline.DB.Register(
    db.AuditLog(sink, db.WithChanges()),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    // No AtPosition — defaults to Before.
)
```

**Important**: `WithChanges()` requires the middleware to run at
`maniflex.Before` (the default position), not `maniflex.After`. The middleware
needs to read the row state *before* the DB step writes, so the diff has
both sides.

With `WithChanges()`, `Changes` is populated as:

| Operation | `Changes` map |
|---|---|
| Create | `{field: {from: null, to: new_value}}` for each non-default field |
| Update | `{field: {from: old, to: new}}` for each changed field |
| Delete | `{field: {from: value, to: null}}` for each field on the pre-image |

Fields that didn't change between pre-image and post-image are omitted.
Fields excluded from the diff (see below) are also omitted.

## Excluding fields from the diff

`WithExcludeFields("password", "api_key", "session_token")` keeps named
fields out of the `Changes` map:

```go
server.Pipeline.DB.Register(
    db.AuditLog(sink, db.WithChanges(), db.WithExcludeFields(
        "password", "ssn", "api_token",
    )),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

Use this for secrets that shouldn't reach the audit pipeline even in
hashed form. Field names are matched against the DB column name (e.g.
`api_token`, not `apiToken`).

`hidden`, `writeonly`, and `encrypted` fields are excluded automatically;
`WithExcludeFields` is for things that don't carry one of those tags but
still need to be redacted.

## Common sinks

The sink interface is small enough to wire to anything that records
structured events.

### Database table

```go
type DBAuditSink struct{ db *sql.DB }

func (s *DBAuditSink) Write(ctx context.Context, r db.AuditRecord) error {
    changes, _ := json.Marshal(r.Changes)
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO audit_logs
            (timestamp, model, operation, resource_id, actor,
             tenant_id, request_id, trace_id, service_name, changes)
        VALUES (?,?,?,?,?,?,?,?,?,?)`,
        r.Timestamp, r.Model, r.Operation, r.ResourceID, r.Actor,
        r.TenantID, r.RequestID, r.TraceID, r.ServiceName, string(changes),
    )
    return err
}
```

A separate table — or a separate database — keeps audit volume from
affecting the operational schema.

### Structured logger

```go
type LogSink struct{ log *slog.Logger }

func (s *LogSink) Write(ctx context.Context, r db.AuditRecord) error {
    s.log.LogAttrs(ctx, slog.LevelInfo, "audit",
        slog.String("model", r.Model),
        slog.String("operation", string(r.Operation)),
        slog.String("actor", r.Actor),
        slog.String("request_id", r.RequestID),
        slog.Any("changes", r.Changes),
    )
    return nil
}
```

The simplest sink — ships every audit event to the same log aggregator
the rest of the app uses. Good for cold-storage compliance archives.

### Async queue

For high-volume systems where the sink might back up, push records to a
durable queue and process them out of band:

```go
func (s *KafkaSink) Write(ctx context.Context, r db.AuditRecord) error {
    b, _ := json.Marshal(r)
    return s.producer.Produce(ctx, "audit-events", b)
}
```

A failed publish is logged but does not fail the request; the queue
itself provides retry semantics.

## Failure semantics

The middleware:

1. Reads the pre-image (when `WithChanges()` is set) before the DB step.
2. Calls `next()`.
3. Checks the result. If `next()` returned a non-nil error, the audit
   record is **not** written — we don't audit failed operations.
4. Checks `ctx.Response`. If status is `>= 400`, again no audit record.
5. Builds the record from the captured pre-image and `ctx.DBResult`.
6. Spawns a goroutine that calls `sink.Write` with a 5-second background
   context.

This means:

- **A failed write produces no audit entry.** The framework's other
  observability — request logs, error metrics — covers failed attempts.
- **A successful write whose audit sink fails still succeeds.** The
  audit record is lost.
- **The audit write outlives the request context.** A long-running
  audit write doesn't block the HTTP response.

For at-least-once delivery, the sink must be backed by durable storage
(a database, a queue) — the in-process goroutine can be lost if the
process is killed before `Write` returns.

## Audit log + versioning

Both record changes. Choose by where the records live and how they're
read:

| Concern | Audit log | Versioning |
|---|---|---|
| Storage | external sink | same DB, sibling table |
| Per-record reconstruction | no | yes (snapshot) |
| Compliance archive | yes | possible but awkward |
| Forensic forensics across the whole system | yes | per-model only |
| Cost | sink-dependent | one extra INSERT per write |

In a production system both are common: audit log feeds a SIEM for
"who did what across everything", versioning provides per-record history
inside the app.

## Operational checklist

- Pick a sink that matches your audit volume: database table for low
  volume, structured logs for medium, durable queue for high.
- Register at `maniflex.Before` when using `WithChanges()`, at
  `maniflex.After` otherwise.
- `WithExcludeFields` every secret column that isn't already
  `writeonly` / `hidden` / `encrypted`.
- Treat the sink as best-effort. Don't rely on the in-process goroutine
  for legal-grade audit retention; use a sink whose own storage is
  durable.
- Index your audit table on `timestamp`, `actor`, `model`, and
  `resource_id` — they are the columns most queries filter by.
- Restrict who can read the audit table. The diffs may contain values
  the original endpoint hid behind `RedactField`.
