# Validate Middleware

The `maniflex/middleware/validate` package supplies validators that go beyond what
`mfx:` tags can express. Each one runs on the **Validate** step alongside the
built-in tag enforcement, and most abort with `422 VALIDATION_ERROR` on rejection.
The exceptions:

- `RestrictField`/`FieldRole` answer `403 FIELD_FORBIDDEN`, because they gate on
  *who is asking*, not on whether the value is valid.
- `RequireLocale` answers `422 MISSING_LOCALE` — still a 422, but with its own
  code so a missing translation is distinguishable from a plain validation miss.
- `UniqueField`'s happy path is a `422`, but if the underlying count query itself
  fails it answers `500 UNIQUE_CHECK_FAILED` rather than letting a duplicate slip
  through.

## `UniqueField`

Rejects a create or update whose value collides with an existing row.

```go
import "github.com/xaleel/maniflex/middleware/validate"

server.Pipeline.Validate.Register(
    validate.UniqueField(sqlDB, maniflex.Postgres, "email"),
    maniflex.ForModel("User"),
)
```

The middleware runs a count query against the underlying database before the
DB step. Compared with the `mfx:"unique"` schema hint, it produces a structured
422 with the offending field instead of a 409 from a constraint violation
later.

The `driver` argument selects the placeholder dialect (`maniflex.Postgres` →
`$N`, `maniflex.SQLite` → `?`) and must match the driver used to open the
adapter. The third argument is the **JSON** field name; it is resolved to the
underlying DB column via `ctx.Model.FieldByJSONName`.

## `RegexField`

Validates that a string field matches a regular expression:

```go
server.Pipeline.Validate.Register(
    validate.RegexField("phone", `^\+?[0-9]{7,15}$`),
    maniflex.ForModel("Contact"),
)
```

A non-matching value aborts with `422` and the field name in `details`.

## `ForbiddenValues`

Rejects writes that contain any of the listed values for a field. Use it for
defence-in-depth on enum-like fields where the mfx enum tag would still allow
a privileged value:

```go
server.Pipeline.Validate.Register(
    validate.ForbiddenValues("role", "superadmin", "root"),
    maniflex.ForModel("User"),
)
```

## `RequireAtLeastOne`

Ensures at least one of the named fields is present in the request body. Most
useful on `OpUpdate`, where every field is otherwise optional:

```go
server.Pipeline.Validate.Register(
    validate.RequireAtLeastOne("name", "email"),
    maniflex.ForOperation(maniflex.OpUpdate),
)
```

## `RequireLocale`

Ensures a localised (`LocaleString`) field carries a non-empty value for each of
the required locale keys. Use it for translatable fields where certain languages
are mandatory:

```go
server.Pipeline.Validate.Register(
    validate.RequireLocale("name", "en", "ar"),
    maniflex.ForModel("Department"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

The field value may arrive as a JSON object (`map[string]any`) or a Go-native
`map[string]string`. A locale key that is absent, `null`, or empty fails the
check. When any required locale is missing the request is aborted with
`422 MISSING_LOCALE`, and `details` lists every offending locale keyed by field.
If the field is absent from the body, `null`, or not a locale map at all, the
rule passes silently — pair it with `required` when presence itself is mandatory.

## `NumericPrecision`

Enforces decimal precision and scale on a numeric field. The check is a
string-parse, so it works regardless of how the column is stored
(`INTEGER`, `NUMERIC(p,s)`, `TEXT`, custom `SQLTyper`):

```go
server.Pipeline.Validate.Register(
    validate.NumericPrecision("amount", 19, 4), // up to 19 total digits, max 4 after the point
    maniflex.ForModel("Invoice"),
)
```

- `precision` — maximum total significant digits (integer + fractional). Pass `0` to disable.
- `scale` — maximum digits after the decimal point. Pass `0` to disable.

Sign (`+`/`-`), leading zeros, and trailing fractional zeros do not count
toward either limit. Scientific notation (`1e3`) is rejected because its
implied precision is ambiguous — supply financial values in plain form.
Absent and `null` values are skipped; pair with `required` when presence
matters.

## `CrossFieldValidate`

A general-purpose escape hatch for rules that span multiple fields:

```go
server.Pipeline.Validate.Register(
    validate.CrossFieldValidate(func(body map[string]any) error {
        if body["status"] == "scheduled" && body["scheduled_at"] == nil {
            return fmt.Errorf("scheduled_at is required when status is scheduled")
        }
        return nil
    }),
    maniflex.ForModel("Post"),
)
```

The returned error becomes the `message` of a `422 VALIDATION_ERROR` response.

## `DateRange`

Ensures an end field is not before a start field. Accepts RFC3339 timestamps
(`"2026-05-01T08:00:00Z"`) and `YYYY-MM-DD` date strings. If either field is
absent, `null`, or unparseable, the rule passes silently — pair with `required`
or another rule when presence matters.

```go
server.Pipeline.Validate.Register(
    validate.DateRange("start_date", "end_date"),
    maniflex.ForModel("Booking"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

Equal dates are accepted (`start == end`). A `422 VALIDATION_ERROR` is returned
with the end field named in `details` when the end precedes the start.

## `RequireWhen`

Makes a field required only when other fields satisfy all listed conditions.
Each condition is a `"field:op:value"` string; op is one of `eq`, `ne`, `gt`,
`gte`, `lt`, `lte`. Multiple conditions are ANDed. Invalid syntax panics at
startup so misconfiguration is caught before the first request.

```go
// Require rejection_reason whenever status is "rejected"
server.Pipeline.Validate.Register(
    validate.RequireWhen("rejection_reason", "status:eq:rejected"),
    maniflex.ForModel("Claim"),
)

// Require shipping_address only for physical orders with priority >= 3
server.Pipeline.Validate.Register(
    validate.RequireWhen("shipping_address", "order_type:eq:physical", "priority:gte:3"),
    maniflex.ForModel("Order"),
)
```

When the conditions are met but the target field is absent, `null`, or empty
string, the request is rejected with `422 VALIDATION_ERROR`. When the
conditions are not all met, the rule passes regardless of the target field's
value — it does not prevent the field from being supplied.

Numeric comparisons (`gt`, `gte`, `lt`, `lte`) coerce both the body value and
the condition value to `float64`. Non-numeric body values cause the condition to
evaluate as false (the target field stays optional).

## `FieldRole` / `RestrictField`

Gate a field on **who is writing it**. These are the write-side twin of
[`response.RedactField`](response.md#redactfield) — the same predicate shape, on
the opposite step.

```go
// Only a superuser may write status; everyone else may write the rest of the row
server.Pipeline.Validate.Register(
    validate.FieldRole("subscription_expires_at", "superuser"),
    maniflex.ForModel("User"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

`RestrictField` takes a predicate instead of a role list, for gates roles cannot
express (ownership, tenant, plan tier):

```go
server.Pipeline.Validate.Register(
    validate.RestrictField("document_quota_bytes",
        func(ctx *maniflex.ServerContext) bool {
            return ctx.HasRole("superuser") || isBillingAdmin(ctx)
        }),
    maniflex.ForModel("User"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

`FieldRole(field, roles...)` is exactly `RestrictField` with a `ctx.HasRole`
predicate (OR-semantics). With **no roles passed it rejects every write** of the
field, so an accidentally empty list fails closed — matching `auth.RequireRole`.

### Why this isn't `readonly`

The `mfx:` write controls are static: `readonly`, `immutable`, `hidden` apply to
every caller identically. They cover *"no client ever writes this"*. They cannot
express *"only a superuser may set `subscription_expires_at`, while the owner
writes the rest of their own row"* — which, without this, costs a separate
endpoint per privileged field.

### Refused, not stripped

A caller without permission gets **`403 FIELD_FORBIDDEN`** naming the field.
This deliberately differs from `readonly`, which silently drops the field:

| | `readonly` | `FieldRole` |
|---|---|---|
| meaning | nobody writes this, ever | *someone* writes this — not you |
| client sending it | confused about the schema | making a privilege error |
| on violation | field dropped, `200` | whole write refused, `403` |

Answering `200` to a write that did not happen, echoing the old value back, is
indistinguishable from success. The write is refused **whole** — a mixed
`PATCH {"title": …, "status": …}` from a non-holder changes neither field.

### Details

- **Only a field present in the body is gated.** A PATCH that does not mention it
  passes untouched. An explicit `null` *is* a write, and is gated.
- **Create and update both**, when you scope it with `ForOperation`.
- `field` is the **JSON** name.
- **Scope it with `maniflex.ForModel`.** Registered without one it applies to
  every model, and a gate naming a field the model does not have can never fire.
  It warns once per model in that case, since a typo would otherwise leave the
  real field ungated in silence.
