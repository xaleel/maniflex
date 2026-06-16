# Workflow Middleware

`maniflex/middleware/workflow` enforces a state-machine on a model's status
field. Declare the permitted transitions once; the middleware rejects writes
that would move a record between states the workflow does not allow, or that
fail a guard (e.g. role check).

```go
import "github.com/xaleel/maniflex/middleware/workflow"

sm := workflow.New("status",
    workflow.Allow("draft",     "submitted"),
    workflow.Allow("submitted", "approved", workflow.RequireRole("manager")),
    workflow.Allow("submitted", "rejected", workflow.RequireRole("manager")),
    workflow.Allow("approved",  "paid",     workflow.RequireRole("finance")),
    workflow.AllowAny(workflow.RequireRole("admin")),    // admin escape hatch
    workflow.AllowInitial("draft", "submitted"),         // legal seed states on Create
)

server.Pipeline.Validate.Register(
    sm.Middleware(),
    maniflex.ForModel("Invoice"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

## How it runs

The middleware lives on the **Validate** step.

- **On `OpCreate`** — if `AllowInitial` is declared, the value of the chosen
  field must be in the set; otherwise any initial value passes. No guards
  apply on Create (the Create itself is its own authorisation surface).
- **On `OpUpdate`** — if the body does not include the status field, the
  middleware is a no-op. Otherwise it reads the current record via
  `ctx.GetModel(modelName).Read(id)` (so reads participate in `ctx.Tx` when
  active), extracts `from`, compares to the body's `to`, and:
  - **same-state** writes (`from == to`) pass silently;
  - the first matching rule wins; its guards run in order;
  - the first guard error rejects the transition with `422 INVALID_TRANSITION`.

A PATCH that triggers the read also costs one round-trip. Stash the loaded
record on `ctx.Set` if you need it again later in the request.

## Rules

| Option | Effect |
|---|---|
| `Allow(from, to, guards...)` | permit `from → to`; both sides may be `"*"` for "any" |
| `AllowAny(guards...)` | shorthand for `Allow("*", "*", guards...)` |
| `AllowInitial(states...)` | restrict Create to the listed initial states |

Rule matching is a linear scan in declaration order — write narrow rules
before broad ones if you want them to take precedence.

## Guards

```go
type Guard interface {
    Check(ctx *maniflex.ServerContext, from, to string) error
}
```

- `RequireRole(roles ...string)` — pass if the caller holds any one of the
  listed roles. OR-semantics; passing zero roles always rejects (a
  defensive choice against accidentally unguarded "require" rules).
- `GuardFunc` — adapt any `func(ctx, from, to) error` to the interface.

A non-nil guard error becomes the response message:

```json
{
  "error": {
    "code": "INVALID_TRANSITION",
    "message": "role [manager] required for transition \"submitted\" → \"approved\"",
    "details": [{"field": "status", "from": "submitted", "to": "approved"}]
  },
  "status": 422
}
```

## Status field type

Values are compared as strings via `fmt.Sprintf("%v", v)`, matching
`validate.ForbiddenValues`. This covers `string`, `int`, and custom enum
types.
