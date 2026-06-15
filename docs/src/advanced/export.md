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

The endpoint reuses the full request pipeline — Auth, tenancy, soft-delete,
and any other middleware registered on `ForOperation(maniflex.OpList)` should
also list `OpExport`:

```go
server.Pipeline.Auth.Register(
    auth.JWTAuth(secret, auth.JWTOptions{}),
    maniflex.ForOperation(maniflex.OpList, maniflex.OpExport),
)
```

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

Computed fields registered via `Server.AddComputedField` are **not** included
yet — they require runtime evaluation per row and the export is read-only at
the storage layer.

## Row cap

`MaxExportRows` (default 100,000) bounds the result. Requests whose filtered
result would exceed the cap return `413 Request Entity Too Large` with a
suggestion to tighten the filters; no partial data is written. Increase the
cap if you have a deliberate need; consider an async job (`pkg/jobs`) for
multi-million-row exports.

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
- **Localised column headers**. Headers always use the JSON field name.
- **Style hints (number formats, frozen headers, autofilter)**. The XLSX is
  intentionally plain.

Open an issue if any of these matter for your use case.
