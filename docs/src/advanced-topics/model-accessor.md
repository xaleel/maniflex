# Model Accessor

The generated REST routes expose a model over HTTP. Application code (action
handlers, middlewares, event listeners, etc.) frequently needs to read or
write a _different_ model than the one the request targets. The model
accessor is the in-process door for that work.

`ctx.GetModel(name)` returns an object exposing the five standard CRUD
operations bound to one registered model. Every call routes through the active
transaction, respects per-model database overrides, and returns the same
`map[string]any` shape the REST layer uses without the HTTP round-trip. The
main difference is the interaction with middlewares, [more on that below](#relationship-to-pipeline-middleware).

```go
row, err := ctx.GetModel("User").Read(userID)
if err != nil {
    return err
}
name := row["name"].(string)
```

## The `ModelAccessor` interface

`ctx.GetModel(name)` yields a `*ModelAccessor`. All methods use
`map[string]any`, keyed by JSON field name, matching the request body and
response envelope.

| Method   | Signature                                                        | Returns                   |
| -------- | ---------------------------------------------------------------- | ------------------------- |
| `List`   | `List(q *QueryParams) ([]map[string]any, error)`                 | A page of rows            |
| `Read`   | `Read(id string) (map[string]any, error)`                        | One row, or `ErrNotFound` |
| `Create` | `Create(data map[string]any) (map[string]any, error)`            | The stored row            |
| `Update` | `Update(id string, data map[string]any) (map[string]any, error)` | The updated row           |
| `Delete` | `Delete(id string) error`                                        | —                         |

```go
// List - q may be nil (page 1, limit 20, no filters or sorts).
admins, err := ctx.GetModel("User").List(&maniflex.QueryParams{
    Filters: []*maniflex.FilterExpr{
        {Field: "role", Operator: maniflex.OpEq, Value: "admin"},
    },
    Page:  1,
    Limit: 100,
})

// Create - returns the stored representation, id and defaults populated.
created, err := ctx.GetModel("User").Create(map[string]any{
    "name":     "Carol",
    "email":    "carol@example.com",
    "role":     "viewer",
    "password": "secret",
})

// Update - a partial patch; only the supplied keys are written.
updated, err := ctx.GetModel("User").Update(userID, map[string]any{
    "role": "admin",
})

// Delete - soft-deletes when the model opts into soft delete, else hard-deletes.
err := ctx.GetModel("User").Delete(userID)
```

`List` accepts the same [`QueryParams`](../using-the-api/querying.md) the query
parser builds from `?filter=` / `?sort=` / `?page=`; filters, sorts, and
pagination all apply. `Read`, `Update`, and `Delete` return
`maniflex.ErrNotFound` when the id is absent. `Create` and `Update` return a
`*maniflex.ErrConstraint` on unique or check violations, allowing DB errors to
be mapped to HTTP responses the same way the pipeline does (see
[Error Handling](../the-request-pipeline/errors.md)).

## Errors surface on first use

`GetModel` never returns an error directly. When the name is not registered — or
the registry is not wired onto the context — it returns an _error accessor_ that
surfaces the failure on the first method call rather than at construction:

```go
_, err := ctx.GetModel("NoSuchModel").List(nil)
// err: maniflex: model "NoSuchModel" is not registered
```

This keeps call sites terse (`ctx.GetModel("User").Read(id)`) without a nil check
on the accessor; the error is handled at the method call as usual.

## Transactions

Accessor operations route through `ctx.Tx` whenever a transaction is active, so
work performed through the accessor commits or rolls back atomically with the
rest of the request:

```go
tx, err := ctx.BeginTx(ctx.Ctx, nil)
if err != nil {
    return err
}
ctx.Tx = tx
defer tx.Rollback() // no-op after a successful Commit

if _, err := ctx.GetModel("Order").Create(order); err != nil {
    return err // the deferred Rollback undoes everything
}
if _, err := ctx.GetModel("Inventory").Update(sku, dec); err != nil {
    return err
}
return tx.Commit()
```

> **The accessor must be obtained _after_ `ctx.Tx` is set.** An accessor captures
> whatever `ctx.Tx` holds at the moment `GetModel` is called. Obtaining it
> _before_ `BeginTx` leaves its writes outside the transaction — and under SQLite
> they can deadlock against the open tx. Always call `ctx.GetModel(...)` after
> `ctx.Tx = tx`:
>
> ```go
> ctx.Tx = tx
> orders := ctx.GetModel("Order") // bound to ctx.Tx — safe
> ```

## Per-model database routing

When a model is pinned to a non-default adapter (see
[Database Backends](../deployment/databases.md)), the accessor resolves that
model's own adapter, so a cross-model read reaches the correct database
automatically. The request transaction, however, belongs to the request's
adapter. When the target model lives on a _different_ adapter, the accessor
cannot enlist that transaction and instead runs the operation outside it against
the correct adapter. Within a single adapter, transactional routing behaves
exactly as above.

## Relationship to pipeline middleware

The accessor talks directly to the database adapter (through the active
transaction when one is set). It does **not** run the request pipeline. Response
transforms, dynamic redaction, field-visibility rules, validation, and any other
registered middleware never observe an accessor read or write — they operate on
`ctx.Response`/`ctx.Body`, which the accessor does not populate.

The practical consequence: an accessor returns the record **as stored**, before
any response-layer shaping. Given a redacting transform on the Response stage —

```go
s.Pipeline.Response.Register(
    response.TransformField("secret", func(any) any { return "[redacted]" }),
    maniflex.ForModel("Account"),
    maniflex.AtPosition(maniflex.After),
)
```

— a REST read of `Account` returns `"secret": "[redacted]"`, because the
transform rewrites `ctx.Response.Data` after the DB step. An accessor read does
not:

```go
row, _ := ctx.GetModel("Account").Read(id)
row["secret"] // the raw stored value — NOT "[redacted]"
```

Two further differences follow from bypassing the response marshaller:

- **Keys are DB column names.** Accessor maps are keyed by each field's
  `mfx` DB name, not the JSON name that response middleware and the API envelope
  use. Where the two names differ, the DB name is the one present in the map.
- **`hidden` and `writeonly` fields are included.** The response marshaller
  strips `mfx:"hidden"` and `mfx:"writeonly"` columns from every payload; the
  accessor does not. Their raw values are present in accessor maps.

This is by design — the accessor is trusted, in-process access for application
logic, not a client-facing surface. It does mean that sanitisation implemented in
response middleware (redaction, field hiding, CDN rewriting) is **not** inherited
by data read through the accessor. When accessor output is forwarded outside the
process, that shaping is the caller's responsibility.

## `QueryModel` is the former name for `List`

`ctx.QueryModel(name, q)` remains available and now delegates to
`ctx.GetModel(name).List(q)`. The accessor is preferred: it exposes `Read`,
`Create`, `Update`, and `Delete` on the same object rather than reading alone.

```go
rows, err := ctx.QueryModel("User", nil)      // deprecated
rows, err := ctx.GetModel("User").List(nil)   // preferred
```

## Typed accessor: `map[string]any` → `*T`

The string-named accessor is dynamic — suited to cases where the model name is
data, or where maps are the natural representation. When the type is known at
compile time, the **typed CRUD** free functions provide the same five operations
against concrete structs. They resolve the model from the type parameter, so no
name string must be kept in sync:

```go
users, err := maniflex.List[User](ctx, nil)                 // []*User
u, err := maniflex.Read[User](ctx, id)                      // *User
created, err := maniflex.Create(ctx, &User{Name: "Jane"})   // *User
updated, err := maniflex.Update(ctx, id, &User{Name: "J."}) // *User
err := maniflex.Delete[User](ctx, id)
```

These route through `ctx.Tx`, honour per-model adapters, and return the same
`ErrNotFound` / `ErrConstraint` errors as the string-named accessor — they are
its typed counterpart, not a separate code path.

One difference is significant: **`maniflex.Update[T]` performs a full-record
update.** Every column except `id` is written from the supplied struct, so any
zero-valued field overwrites the stored value. For a partial patch — only the
specified keys — use the map-based `ctx.GetModel(name).Update(id, data)` instead.

## Choosing between the accessors

| Requirement                                       | Accessor                                   |
| ------------------------------------------------- | ------------------------------------------ |
| Dynamic access where the model name is a variable | `ctx.GetModel(name)`                       |
| Concrete structs and compile-time field names     | `maniflex.List[T]` / `Read[T]` / …         |
| A partial patch (only some fields)                | `ctx.GetModel(name).Update(id, data)`      |
| A full-record replace from a struct               | `maniflex.Update[T](ctx, id, &rec)`        |
| Joins, aggregates, custom SQL                     | [Raw Queries & Aggregates](raw-queries.md) |

Both accessors serve **in-process** cross-model work inside a request. Exposing a
model over HTTP is a matter of registering it and letting the generated routes
handle it; the accessor is the tool a handler reaches for once execution is
already inside the pipeline.
