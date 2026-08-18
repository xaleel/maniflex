# Error Handling

Every error returned by the pipeline is delivered to the client as the same
JSON envelope. This page describes that envelope, the sentinel errors the
framework recognises, and how to produce errors from middleware.

## The error envelope

A failing request writes:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
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
| `400` | `BODY_READ_ERROR` | an I/O failure while reading the request body |
| `400` | `INVALID_QUERY` | unknown filter/sort field, malformed `?include`, etc. |
| `400` | `MULTIPART_ERROR` | malformed `multipart/form-data` |
| `404` | `NOT_FOUND` | record does not exist (or is soft-deleted) |
| `409` | `CONFLICT` | unique or check constraint violated |
| `413` | `BODY_TOO_LARGE` | request body exceeded the 4 MB read limit |
| `422` | `VALIDATION_ERROR` | one or more `mfx:` tag rules failed |
| `500` | `DB_ERROR` | unclassified adapter error |
| `500` | `TX_BEGIN_ERROR` / `TX_COMMIT_ERROR` | transaction lifecycle failure |
| `499` | _(no body)_ | the client disconnected before the response was written |
| `501` | `NO_STORAGE` | file endpoint hit with no `FileStorage` configured |
| `504` | `TIMEOUT` | `Config.QueryTimeout` (or another server-side deadline) expired |

A panic anywhere in the pipeline is caught and reported as `500 PANIC` by the
framework's recoverer — with one deliberate exception. A handler that panics with
`http.ErrAbortHandler` is abandoning its response on purpose (this is what
`httputil.ReverseProxy` does when an upstream dies mid-stream), so the recoverer
passes it on and `net/http` closes the connection silently. It is not logged as a
panic, and no error envelope is appended to whatever was already written.

`499` is nginx's non-standard "Client Closed Request" (`maniflex.StatusClientClosedRequest`).
Nothing is written to the client — the connection is already gone — but the status
is what your access log, metrics, and any After middleware reading the response
status will see, so a caller who hangs up is not counted as a server timeout.
Only a genuine server-side deadline produces `504 TIMEOUT`. The disconnect itself
is logged at `DEBUG`, not as an error.

## Aborting from middleware

The standard way to produce an error from middleware is `ctx.Abort`:

```go
ctx.Abort(statusCode, code, message)
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

For a 5xx status, `message` is a private diagnostic: Maniflex logs it once
through `Config.Logger`, correlated by `request_id`, but sends only generic
status text such as `internal server error` or `bad gateway`. The HTTP status
and stable error `code` remain public. Any `details` on a directly assigned
5xx `APIResponse` are also removed at the wire boundary. Validated 4xx
messages and details are unchanged. Every routed response echoes the
correlation value in `X-Request-Id`.

## Returning structured details

For per-field errors and similar payloads, set `ctx.Response` directly:

```go
ctx.Response = &maniflex.APIResponse{
    StatusCode: http.StatusUnprocessableEntity,
    Error: &maniflex.APIError{
        Code:    "VALIDATION_ERROR",
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
    Kind    ConstraintKind  // unique, foreign_key, or not_null
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

## Every exported sentinel

`ErrNotFound` and `ErrConstraint` above are the two you match in a handler. The
rest are listed here because they are part of the compatibility contract: a
sentinel keeps its identity for all of v1, so `errors.Is` against any of them
goes on working. Match on these values, never on message text — the text is
explicitly **not** covered (see
[Stability & Compatibility](../reference/compatibility.md)).

Core module, `github.com/xaleel/maniflex`:

| Sentinel | Returned when |
|---|---|
| `ErrNotFound` | the row does not exist, or is soft-deleted |
| `*ErrConstraint` | a unique or check constraint was violated — a type, not a value; use `errors.As` |
| `ErrNoAdapter` | `BeginTx` is called with no database adapter configured |
| `ErrRawNotSupportedInTx` | `RawQuery`/`RawExec` run inside a transaction whose `Tx` cannot execute raw SQL. Refused rather than silently run outside the transaction |
| `ErrIncrementOutOfBounds` | `Increment` would take a column past its `mfx:"min:"`/`"max:"` bound. Distinct from `ErrNotFound`: only this one may succeed on a retry |
| `ErrIncrementNotSupported` | the adapter does not implement `Incrementer` |
| `ErrFileNotFound` | `FileStorage.Retrieve` is given a key that does not exist |
| `ErrPresignUnsupported` | the storage backend cannot mint a presigned upload; the upload-url route answers `501` |
| `ErrAlreadyStarted` | a start method is called while the server already has an active startup owner |
| `ErrStopped` | a start method is called after the server stopped or failed. A server is not restartable — build a new one |
| `ErrRegistrationClosed` | a route or spec contributor is registered after `Start`/`Handler` sealed the server |

Subpackages:

| Sentinel | Package | Returned when |
|---|---|---|
| `ErrBusClosed` | `events/inproc` | `Publish` after `Close` — the event was never accepted |
| `ErrQueueFull` | `events/inproc` | a matching subscription's queue is full. The bus is behind; the caller still holds the event |
| `ErrDrainIncomplete` | `events/inproc` | `Close` gave up with deliveries still running |
| `ErrUnauthorized` | `realtime` | an `Authenticator` rejected a connection — a type, carrying `Reason` |
| `ErrResponseTooLarge` | `pkg/integration` | an upstream response exceeded `MaxResponseBytes` |
| `ErrCrossOriginRedirect` | `pkg/integration` | the redirect policy refused to forward the request, and its headers, to another origin |
| `ErrHTTPStatus` | `pkg/integration` | the upstream answered non-2xx — a type, carrying the status and decoded body |
| `ErrWebhookReplay` | `pkg/integration` | a signed webhook was already accepted; the receiver maps it to `409` |
| `ErrImbalanced` | `pkg/ledger` | debits do not equal credits for some currency in the entry |
| `ErrNoLines` | `pkg/ledger` | `Post` was called with fewer than two lines |
| `ErrCurrencyMismatch` | `pkg/money` | `Add`/`Sub` were given amounts in different currencies |

`TestErrorsDocListsEveryExportedSentinel` fails when an exported `Err*`
identifier is missing from this page, so a new one cannot ship undocumented.

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
ctx.Abort(http.StatusBadGateway, "PAYMENT_PROVIDER_ERROR", resp.Message)
return nil
```

`Abort` logs the supplied 5xx message automatically. The explicit log is still
useful when you have structured provider fields that should not be flattened
into the diagnostic string. Neither `resp.Message` nor those fields reach the
client.

## Next

- **[ServerContext](context.md)** — the full set of fields available to error-producing middleware.
- **[Transactions](transactions.md)** — rollback semantics.
- **[Writing Middleware](middleware.md)** — where to attach error-producing logic.
