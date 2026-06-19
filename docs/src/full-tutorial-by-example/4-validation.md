# 4. Validation & Business Rules

The `mfx:` tag rules from Part 3 cover the common case — required fields,
numeric ranges, enums, uniqueness hints. Anything that goes beyond a single
field belongs in Validate-step middleware. This part adds three rules:

1. ISBNs must be 13 digits with hyphens optional.
2. A user may not review the same book twice.
3. A user must have bought a book before reviewing it.

## Field-format validation

`validate.RegexField` is enough for the ISBN check:

```go
import "github.com/xaleel/maniflex/middleware/validate"

server.Pipeline.Validate.Register(
    validate.RegexField("isbn", `^(?:97[89])?\d{10}$`),
    maniflex.ForModel("Book"),
)
```

The middleware runs after the `mfx:` tag rules. A malformed ISBN aborts the
request with `422 VALIDATION_FAILED` and the field in `details`.

We strip hyphens before validating so the client can send the human-readable
form. A small Service-step middleware does the rewrite:

```go
server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    if raw, ok := ctx.Field("isbn"); ok {
        if v, ok := raw.(string); ok {
            ctx.SetField("isbn", strings.ReplaceAll(v, "-", ""))
        }
    }
    return next()
}, maniflex.ForModel("Book"), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
```

Order matters: Validate runs before Service in the pipeline. We're rewriting
*after* validation has confirmed the cleaned-up format would pass. To
validate the cleaned value, swap the registration so the cleanup is on
Validate at `maniflex.Before` (the default).

## "One review per book per user"

Two flavours of uniqueness:

- **Schema uniqueness** (`mfx:"unique"`) — adds a `UNIQUE` constraint on a
  single column. Good for an email address.
- **Cross-column uniqueness** — needs custom validation, because no single
  column is unique.

For reviews we need both `book_id` and `user_id` to be unique together. A
small middleware that consults the database:

```go
server.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    bookID, _ := ctx.Field("book_id")
    rows, err := ctx.RawQuery(
        `SELECT id FROM reviews
          WHERE book_id = ? AND user_id = ? AND deleted_at IS NULL`,
        bookID, ctx.Auth.UserID,
    )
    if err != nil {
        return err
    }
    if len(rows) > 0 {
        ctx.Abort(http.StatusConflict, "ALREADY_REVIEWED",
            "you have already reviewed this book")
        return nil
    }
    return next()
}, maniflex.ForModel("Review"), maniflex.ForOperation(maniflex.OpCreate))
```

Two things to notice:

- We use `ctx.RawQuery` rather than `ctx.GetModel(...).List` because we need
  a count, not the rows. Either works.
- The `user_id` we check is `ctx.Auth.UserID`, not the body's
  `user_id`. In the next section we'll force the body field to match.

## "Body owner must match authenticated user"

Letting a client supply `user_id` is asking for impersonation. `service.OwnerScope`
from the catalogue forces the field on every create:

```go
import "github.com/xaleel/maniflex/middleware/service"

server.Pipeline.Service.Register(
    service.OwnerScope("user_id"),
    maniflex.ForModel("Review"), maniflex.ForOperation(maniflex.OpCreate),
)
```

`OwnerScope` reads `ctx.Auth.UserID` and sets it on the body via `ctx.SetField`,
overwriting whatever the client sent. A client who omits the field gets it
filled in; a client who sets it to someone else's ID has the value
overwritten silently.

This is a good place to apply [Forbidden values](../middleware-catalogue/validate.md#forbiddenvalues)
on `role` for `User` too — defence in depth against a privilege-escalation
payload:

```go
server.Pipeline.Validate.Register(
    validate.ForbiddenValues("role", "admin"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
)
```

A normal sign-up cannot self-promote to admin; an admin still can, because
they go through `PATCH` not `POST`, and the rule is scoped to create only.

## Cross-field rules

The third rule — review only books you've bought — depends on a model
(`Order`) we haven't built yet. We come back to it in Part 7 once orders
exist, using the same `Validate.Register` shape with a join query:

```go
server.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    bookID, _ := ctx.Field("book_id")
    rows, _ := ctx.RawQuery(
        `SELECT 1
           FROM order_lines ol
           JOIN orders o ON o.id = ol.order_id
          WHERE o.customer_id = ?
            AND ol.book_id    = ?
            AND o.status     IN ('paid','shipped')`,
        ctx.Auth.UserID, bookID,
    )
    if len(rows) == 0 {
        ctx.Abort(http.StatusForbidden, "PURCHASE_REQUIRED",
            "you may only review books you have bought")
        return nil
    }
    return next()
}, maniflex.ForModel("Review"), maniflex.ForOperation(maniflex.OpCreate))
```

We leave this in the codebase as a stub for now and complete it in Part 7.

## Where each rule lives

| Rule | Where | Why |
|---|---|---|
| ISBN format | Validate (catalogue) | format check on one field |
| One-review-per-book | Validate (custom) | queries another row |
| `user_id` belongs to caller | Service (catalogue) | mutates body |
| `role` cannot be admin on sign-up | Validate (catalogue) | rejects body value |
| Must have purchased | Validate (custom, deferred) | cross-model query |

The general rule: **field-level rules go in Validate; rules that mutate the
body go in Service; rules that need the row to exist go in After-DB**.

## Next

In **[Part 5 — File Uploads](5-uploads.md)** we add a cover image to
each book, served from local storage during development and ready to swap
for S3 in production.
