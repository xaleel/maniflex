# Validate Middleware

The `maniflex/middleware/validate` package supplies validators that go beyond what
`mfx:` tags can express. Each one runs on the **Validate** step alongside the
built-in tag enforcement and aborts with `422 VALIDATION_FAILED` on rejection.

## `UniqueField`

Rejects a create or update whose value collides with an existing row.

```go
import "maniflex/middleware/validate"

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

The returned error becomes the `message` of a `422 VALIDATION_FAILED` response.

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
