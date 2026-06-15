# Error Handling

Every error returned by the pipeline is delivered to the client as the same
JSON envelope. This page describes that envelope, the sentinel errors the
framework recognises, and how to produce errors from middleware.

## The error envelope

A failing request writes:

```json
{
  "error": {
    "code": "VALIDATION_FAILED",
    "message": "field \"email\" is required",
    "details": { /* optional, per-error */ }
  }
}
```

with the HTTP status code from the underlying failure. The `code` is a short
machine-readable identifier; `message` is a human-readable summary; `details`
is optional and may carry per-field errors or other structured context.

A successful request uses the `{"data": ...}` envelope instead; the two are
mutually exclusive.

## Built-in error responses

The default pipeline produces the following errors without any user code:

| Status | Code | Source |
|---|---|---|
| `400` | `INVALID_JSON` | malformed JSON body |
| `400` | `EMPTY_BODY` | empty body on `POST` / `PATCH` |
| `400` | `BODY_READ_ERROR` | body exceeded the 4 MB read limit |
| `400` | `INVALID_QUERY` | unknown filter/sort field, malformed `?include`, etc. |
| `400` | `MULTIPART_ERROR` | malformed `multipart/form-data` |
| `404` | `NOT_FOUND` | record does not exist (or is soft-deleted) |
| `409` | `CONFLICT` | unique or check constraint violated |
| `422` | `VALIDATION_FAILED` | one or more `mfx:` tag rules failed |
| `500` | `DATABASE_ERROR` | unclassified adapter error |
| `500` | `TX_BEGIN_ERROR` / `TX_COMMIT_ERROR` | transaction lifecycle failure |
| `501` | `NO_STORAGE` | file endpoint hit with no `FileStorage` configured |
| `504` | `TIMEOUT` | request context deadline exceeded |

A panic anywhere in the pipeline is caught and reported as `500 PANIC` by the
framework's recoverer.

## Aborting from middleware

The standard way to produce an error from middleware is `ctx.Abort`:

```go
func ctx.Abort(statusCode int, code, message string)
```

It populates `ctx.Response` with an error envelope. The caller must then return
`nil` (or an error) *without* calling `next()`:

```go
if header == "" {
    ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
    return nil
}
```

Subsequent steps are skipped; the Response step writes the prepared envelope.
Calling `next()` after `Abort` allows downstream steps to overwrite the
response — usually not what you want.

## Returning structured details

For per-field errors and similar payloads, set `ctx.Response` directly:

```go
ctx.Response = &maniflex.APIResponse{
    StatusCode: http.StatusUnprocessableEntity,
    Error: &maniflex.APIError{
        Code:    "VALIDATION_FAILED",
        Message: "one or more fields failed validation",
        Details: []map[string]string{
            {"field": "email",    "message": "must be a valid email"},
            {"field": "password", "message": "must be at least 8 characters"},
        },
    },
}
return nil
```

This is the shape used by the default Validate step.

## Sentinel errors from the adapter

Adapter methods return errors that the DB step maps to HTTP responses. The two
that user code most often interacts with are exported as sentinels.

### `maniflex.ErrNotFound`

```go
var ErrNotFound = errors.New("record not found")
```

Returned by `FindByID`, `Update`, and `Delete` when the row does not exist (or
is soft-deleted). Detect it with `errors.Is`:

```go
row, err := ctx.GetModel("Invoice").Read(id)
if errors.Is(err, maniflex.ErrNotFound) {
    ctx.Abort(http.StatusNotFound, "INVOICE_NOT_FOUND",
        fmt.Sprintf("invoice %s does not exist", id))
    return nil
}
```

The default DB step does this conversion automatically; you only need it when
you are calling the adapter yourself from a Service middleware.

### `*maniflex.ErrConstraint`

```go
type ErrConstraint struct {
    Table   string
    Column  string  // may be empty when the driver does not expose it
    Detail  string  // raw driver message
}
```

Returned by `Create` and `Update` on unique or check constraint violations.
Both SQLite and Postgres errors are normalised into this type, so middleware
need not inspect driver-specific codes.

```go
row, err := ctx.GetModel("User").Create(data)
var ec *maniflex.ErrConstraint
if errors.As(err, &ec) {
    ctx.Abort(http.StatusConflict, "DUPLICATE",
        fmt.Sprintf("%s already exists", ec.Column))
    return nil
}
```

## Errors and transactions

When a request runs inside a transaction (see [Transactions](transactions.md))
and any step returns an error or sets `ctx.Response` to status `>= 400`, the
transaction is rolled back before the response is written. The client sees the
error envelope; the database sees no change.

## Logging errors

`ctx.Logger()` returns a `*slog.Logger` pre-seeded with `request_id` and
`trace_id`, so a single log line correlates with the request that produced it:

```go
ctx.Logger().Error("payment provider rejected charge",
    slog.String("provider", "stripe"),
    slog.String("error_code", resp.Code),
)
ctx.Abort(http.StatusBadGateway, "PAYMENT_DECLINED", resp.Message)
return nil
```

Log first, then abort — the log line is what you'll need when debugging.

## Next

- **[ServerContext](context.md)** — the full set of fields available to error-producing middleware.
- **[Transactions](transactions.md)** — rollback semantics.
- **[Writing Middleware](middleware.md)** — where to attach error-producing logic.
