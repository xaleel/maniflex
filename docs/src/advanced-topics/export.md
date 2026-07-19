# CSV / XLSX Export

Models can opt into an auto-generated export endpoint that streams CSV or
XLSX of the same data the standard list endpoint returns, with the same
filter and sort query parameters:

```go
server.MustRegister(Invoice{}, maniflex.ModelConfig{
    ExportEnabled: true,
    MaxExportRows: 50_000, // optional cap; defaults to 100,000
})
```

This mounts:

```
GET /invoices/export             → CSV (default)
GET /invoices/export?format=xlsx → XLSX
```

The endpoint reuses the full request pipeline — Auth, tenancy, soft-delete —
and middleware registered on `ForOperation(maniflex.OpList)` **covers the
export too**. An export is a list in another format, so whatever decides which
rows a caller may list decides which rows they may export:

```go
server.Pipeline.Auth.Register(
    auth.JWTAuth(secret, auth.JWTOptions{}),
    maniflex.ForOperation(maniflex.OpList), // also covers OpExport
)
```

Naming `OpExport` as well is harmless but redundant. The implication runs one
way only: `ForOperation(maniflex.OpExport)` means the export alone, which is
what you want for export-specific middleware such as a rate limiter.

> Before v0.2.5 this was not so — a middleware scoped to `OpList` did not run
> for exports, so tenancy written that way scoped the list and let the export
> return every tenant's rows.

## Query parameters

The same `filter`, `sort`, and `include` parameters work as on `GET /invoices`.
`page` and `limit` are ignored — the export reads every row that matches the
filters, up to `MaxExportRows`.

```
GET /invoices/export?filter=status:eq:posted&sort=created_at:desc
```

## Column selection

Each model field appears as one column, named by its `json` tag (the same
identifier you see in API responses). Hidden, writeonly, and file-typed
columns are excluded:

| Field tag | In export? |
|---|---|
| (default) | yes |
| `hidden` | no |
| `writeonly` | no |
| `file` | no (raw storage keys are useless to the recipient) |

A field masked for this caller by a response middleware
([`response.RedactField`](../middleware-catalogue/response.md#redactfield)) is
excluded too — column and values both, so the export does not advertise a field
it will not fill. Those tags are the same for every caller; a masking middleware
decides per request, and the export honours both.

Computed fields registered via `Server.AddComputedField` are **not** included
yet — they require runtime evaluation per row and the export is read-only at
the storage layer.

## Row cap

`MaxExportRows` (default 100,000) bounds the result. Requests whose filtered
result would exceed the cap return `413 Request Entity Too Large` with a
suggestion to tighten the filters; no partial data is written. Increase the
cap if you have a deliberate need; consider an async job (`pkg/jobs`) for
multi-million-row exports.

## Concurrency cap

An export reads its whole result set into memory and holds it until the last
byte has been written to the client. `MaxExportRows` bounds one export's row
count, but not how wide a row is, and not how many exports run at once — so
the memory an export costs is `rows × width`, and the memory the *server* costs
is that again times however many are in flight. A handful of concurrent exports
of a wide model is enough to exhaust the heap with every one of them
individually inside its cap.

`Config.MaxConcurrentExports` (default 4) bounds the product:

```go
server := maniflex.New(maniflex.Config{
    MaxConcurrentExports: 8, // 0 uses the default of 4; negative disables the limit
})
```

The limit is server-wide rather than per-model, because the heap it protects is
shared. An export arriving when every slot is taken is **rejected immediately**
with `503 Service Unavailable`, code `EXPORT_BUSY`, and a `Retry-After` header —
it is not queued. Queuing would hold the connection open behind work that is
slow by nature, so the caller is told to come back instead.

The slot is taken before the pipeline runs and released when the request
returns, so it spans the database read and the write — the whole window the rows
are live. Note that this means the refusal happens *before* Auth: a request that
would have failed authentication gets the 503 rather than a 401 while the server
is saturated.

Set a negative value to remove the limit if you have admission control in front
of the service.

## Response shape

| Format | `Content-Type` | `Content-Disposition` |
|---|---|---|
| CSV | `text/csv; charset=utf-8` | `attachment; filename="<model>-<ts>.csv"` |
| XLSX | `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet` | `attachment; filename="<model>-<ts>.xlsx"` |

The XLSX writer produces a minimal-but-valid `.xlsx` workbook with one sheet
named `Data`. Strings are written as inline string cells — no shared-string
table, no styles, no formulas — which keeps the writer dependency-free
(stdlib `archive/zip` and `encoding/xml` only) at the cost of slightly
larger files than a heavyweight library would produce.

Unsupported `?format=` values return 400 `INVALID_FORMAT`.

## What's not in v1

- **Async / job-backed exports** for multi-million-row datasets. Today's
  endpoint streams synchronously; the upper bound is `MaxExportRows`.
- **Constant-memory exports.** The CSV and XLSX writers hold nothing per row —
  they serialise and write one row at a time — but the rows themselves are read
  from the database in one go before the write begins, so one export still costs
  memory proportional to its result set. `MaxConcurrentExports` bounds how many
  of those can coexist; a true row-by-row cursor would remove the per-export
  cost too, at the price of pinning a database connection for the length of the
  client's download and turning a mid-stream database error into a silently
  truncated file.
- **Localised column headers**. Headers always use the JSON field name.
- **Style hints (number formats, frozen headers, autofilter)**. The XLSX is
  intentionally plain.

Open an issue if any of these matter for your use case.
