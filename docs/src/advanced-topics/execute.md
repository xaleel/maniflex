# In-Process Invocation (`Execute`)

`server.Action` lets HTTP reach your logic. `server.Execute` is the other half: it
lets your logic reach the pipeline.

```go
res, err := srv.Execute(ctx.Ctx, maniflex.Invocation{
    Model:     "Item",
    Operation: maniflex.OpUpdate,
    ID:        itemID,
    Body:      map[string]any{"status": "approved"},
    Auth:      &approver,   // a typed principal, not a header
    Tx:        ctx.Tx,      // joins the caller's transaction
})
```

Every step runs — Auth, Deserialize, Validate, Service, DB, Response — in the same
order, through the same middleware, as the equivalent request from a client. The
result is the same envelope that request would have produced.

## Why it exists

Without it, HTTP is the only way in, and code that needs to run a model operation
from Go has to make a request to itself:

```go
// Don't do this.
req, _ := http.NewRequest("PATCH", path, body)
req.Header.Set("X-Replay", "1")
req.Header.Set("X-Requester-ID", requesterID)
router.ServeHTTP(httptest.NewRecorder(), req)
```

Both defects that follow are structural, not careless:

- **A principal passed as a header is a principal any client can send.** The gates
  then grow bypasses keyed on that header, and those bypasses are reachable from
  the internet by construction. No amount of care fixes it at that layer — you
  cannot pass an identity to a router except through the request.
- **N requests cannot be one transaction.** A loop that replays N items over HTTP
  commits each separately, so a failure at item 3 leaves 1 and 2 written while the
  batch is marked unfinished — and re-running it re-applies the prefix.

`Invocation.Auth` is a typed `*AuthInfo`, so there is no header to forge and no
bypass to grow. `Invocation.Tx` is your transaction, so N invocations commit or
roll back together.

## Maker–checker, atomically

The use case this was built for: a staged write, captured earlier, executed on
approval as one unit.

```go
tx, err := ctx.BeginTx(ctx.Ctx, nil)
if err != nil {
    return err
}
defer tx.Rollback() // no-op once committed

for _, item := range request.Items {
    if _, err := srv.Execute(ctx.Ctx, maniflex.Invocation{
        Model:     item.Model,
        Operation: maniflex.OpUpdate,
        ID:        item.ResourceID,
        Body:      item.Payload,   // the bytes captured at request time
        Auth:      &requester,     // whoever the write is attributed to
        Tx:        tx,
    }); err != nil {
        return err // the whole batch rolls back — no committed prefix
    }
}
return tx.Commit()
```

A non-2xx answer comes back as an **error**, not a value, which is what makes that
loop correct. `if err != nil { return err }` is the natural Go loop, and it has to
be the one that rolls back — handing a `422` back as `(res, nil)` would make the
naive loop commit items 1 and 2. Inspect the status with `errors.As`:

```go
var execErr *maniflex.ExecuteError
if errors.As(err, &execErr) && execErr.StatusCode == http.StatusNotFound {
    // the record is gone
}
```

The `*APIResponse` is returned either way, so you can read `Data` and `Meta` on a
success and the error envelope on a failure.

## Authentication: `ctx.InProcess()`

An in-process call has no `Authorization` header, because there is no client. The
shipped `auth.JWTAuth` and `auth.JWKSAuth` stand aside when — and only when — the
request came from `Execute` **and** already carries a principal:

```go
if ctx.InProcess() && ctx.Auth != nil {
    return next()   // identity already established, by trusted code
}
```

Everything else on the Auth step still runs against that principal: `RequireRole`,
`RequireOwner`, ABAC policies, your own middleware. An `Execute` with no `Auth` is
**anonymous** and is refused exactly as an anonymous request is — being internal is
not being authenticated.

`ctx.InProcess()` reads an unexported field that only `Execute` sets, so no client
and no middleware can claim it. Over HTTP it is always `false`, and the JWT header
path behaves bit-for-bit as it always has. Use it in your own middleware where a
check is about the transport rather than the caller:

```go
server.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if ctx.InProcess() { return next() }  // no browser to protect from CSRF
    return csrfCheck(ctx, next)
}, maniflex.WithName("csrf"))
```

That is also the supported way to exempt a middleware from in-process calls. There
is deliberately no `SkipMiddlewares` list on `Invocation`: a named list of security
checks to switch off is the shape of the very bypass `Execute` exists to delete,
and middleware names are labels for trace logs, not identifiers (six shipped
middlewares share the name `otel.span`). Writing the exemption at the registration
site keeps it type-checked, greppable, and rename-proof.

## No step can be skipped

There is no `Steps` bitmask. `Execute` runs the whole pipeline, so it cannot become
a quieter way into the database than the HTTP route it mirrors:

- **Validate runs**, so `mfx` rules — `required`, `enum`, `readonly`, `immutable` —
  bind an `Execute` exactly as they bind a request. An `Execute` is not a
  mass-assignment hole.
- **Auth runs**, so authorisation registered there cannot be walked around by
  naming yourself in `Invocation.Auth`.
- **Service runs**, so your business hooks fire.

Skipping Auth in particular would remove the isolation the injected principal is
meant to be constrained by — which is the vulnerability, not the fix.

## `Invocation`

| Field | Meaning |
|---|---|
| `Model` | Registered model's struct name, e.g. `"Order"`. Required. |
| `Operation` | `OpCreate`, `OpRead`, `OpUpdate`, `OpDelete`, `OpList`. Required. |
| `ID` | Primary key. Required for read, update, delete. |
| `Body` | Request body for create/update. `[]byte`/`json.RawMessage`/`string` verbatim; anything else is marshalled. |
| `Query` | `url.Values` carrying filters, sort, include, pagination. |
| `Auth` | The principal. Typed, unforgeable, not a header. |
| `Tx` | Transaction to join. The caller owns it — `Execute` neither commits nor rolls back. |
| `Header` | Request headers for middleware that reads them (`If-Match`, `Accept-Language`). Not `Authorization` — use `Auth`. |

**An update is a PATCH, not a replace.** A field absent from `Body` is left alone,
exactly as over HTTP — so send a map when you mean to patch some columns:

```go
Body: map[string]any{"status": "approved"},  // every other column untouched
```

This is not a re-implementation of presence semantics; `Execute` synthesises the
request and the real Deserialize step binds the body, so the rule is the one the
HTTP path has, not a second copy of it that agrees today and drifts later.

**Refused operations.** `OpExport` and `OpReadAttachment` stream bytes to a
response writer rather than producing a value, so there is nothing to return.
`OpAction` and `OpSearch` run trimmed pipelines of their own — call an action's
handler directly, or `ctx.Search`.

**Registration window.** `Execute` builds the router on first use, exactly as
`Handler()` does, so `Pipeline.Register` and `Server.Action` must come first.

## When to use it

| Need | Use |
|---|---|
| Serve a client | The generated routes |
| Run a model operation from Go, with auth and validation | `Execute` |
| Read or write from Go, without the pipeline | [Typed CRUD / Model Accessor](model-accessor.md) |
| Several operations, one transaction | `Execute` with a shared `Tx`, or [Batch](batch-saga.md) |

Reach for typed CRUD or `ctx.GetModel` when you want the database. Reach for
`Execute` when you want the *rules* — the same auth, validation, hooks, scoping
and response an HTTP caller would get, minus the socket.
