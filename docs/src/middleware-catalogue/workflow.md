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

The middleware lives on the **Validate** step. (A machine that declares
`OnTransition` hooks registers on the **DB** step instead — see
[Transition hooks](#transition-hooks-ontransition).)

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
| `OnTransition(from, to, fn)` | run `fn` inside the write's transaction when `from → to` is taken |

Rule matching is a linear scan in declaration order — write narrow rules
before broad ones if you want them to take precedence.

A machine with **no `Allow` rules permits nothing** — every transition is
rejected. If you want hooks without restricting transitions, say so explicitly
with `AllowAny()`.

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

## Transition hooks (`OnTransition`)

`OnTransition(from, to, fn)` attaches a side effect to a transition. The hook
runs **inside the write's transaction**, so it commits with the transition or
not at all:

```go
sm := workflow.New("status",
    workflow.Allow("pending",   "confirmed", workflow.RequireRole("store_owner")),
    workflow.Allow("confirmed", "pending",   workflow.RequireRole("store_owner")),
    workflow.AllowAny(workflow.RequireRole("admin")),

    workflow.OnTransition("pending",   "confirmed", applyStoreCredit),
    workflow.OnTransition("confirmed", "pending",   reverseStoreCredit),
    workflow.OnTransition("*",         "delivered", emitReviewRequested),
)

server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),
    maniflex.ForModel("Order"),
    maniflex.ForOperation(maniflex.OpUpdate),
)
server.Pipeline.DB.Register(
    sm.Hooks(),
    maniflex.ForModel("Order"),
    maniflex.ForOperation(maniflex.OpUpdate),
)
```

```go
type Hook func(ctx *maniflex.ServerContext, from, to string) error
```

### `Hooks()` replaces `Middleware()` — it does not supplement it

A machine with hooks registers **`sm.Hooks()` on the DB step**, and that is the
only registration it needs: `Hooks()` enforces the same rules and guards
`Middleware()` does, and more strictly. Declaring a hook makes `Middleware()`
panic rather than let it be registered on Validate where the hooks could never
fire.

| Machine | Register | Transaction |
|---|---|---|
| guards only | `sm.Middleware()` on **Validate** | not required |
| any `OnTransition` | `sm.Hooks()` on **DB** | **required** — `WithTransaction` on Service |

The split exists because the two cannot run at the same point. A guard must run
before anything is written; a hook must run inside the write's transaction — and
that transaction does not exist until `WithTransaction` opens it on the Service
step, *after* Validate.

That is also why `Hooks()` re-reads `from` rather than trusting the Validate
step's verdict. The Validate-step read takes no lock, so two concurrent PATCHes
can both observe `from="pending"`, both pass, and **both apply the store
credit**. `Hooks()` re-reads `from` through `ctx.LockForUpdate` inside the
transaction: the loser waits, then sees `confirmed` and is either a no-op
(same-state) or re-checked as the transition it has really become. Because the
re-read can name a *different* rule than the one that passed on the stale value,
the matching rule's guards re-run too.

With no active transaction, `Hooks()` refuses the request with
`500 WORKFLOW_NO_TX` rather than take no lock and silently reintroduce the race
— the same fail-loudly stance as `mfx:"lock_scope"`.

### Semantics

- **Fire-all-matching**, in declaration order — unlike `Allow`, which is
  first-match-wins. `OnTransition("pending", "confirmed", …)` and
  `OnTransition("*", "delivered", …)` are independent side effects, and a rule
  ordering that silenced one of them would be a bug rather than a policy.
- **`OpUpdate` only.** A Create seeds an initial state (`AllowInitial` governs
  it); it is not a transition. Use a Service-step middleware for create-time
  side effects.
- **Same-state writes fire nothing** — a no-op is not a transition.
- **A returned error rolls the whole request back**, transition included, and
  answers `500 WORKFLOW_HOOK_ERROR`. To choose your own status, call
  `ctx.Abort(…)` and return `nil`; the write still rolls back and your response
  is what the client sees.
- The hook runs **after** the write lands, so a read through `ctx.Tx` sees the
  new state.

## Status field type

Values are compared as strings via `fmt.Sprintf("%v", v)`, matching
`validate.ForbiddenValues`. This covers `string`, `int`, and custom enum
types.
