# Startup Validation & Strict Mode

maniflex validates your configuration once, at startup, and reports everything
wrong with it in a single message. Most problems are fatal on their own;
`Config.Strict` adds the handful that are merely suspect.

The principle: **a directive that parses but cannot be honoured is an error, not
a silent no-op.** A misspelt tag that is quietly ignored leaves the protection
you wrote absent, and nothing at runtime says so.

## Always fatal

These fail whether or not strict mode is on. Each one used to warn and then
discard what you asked for.

| Problem | Why it cannot be a warning |
|---|---|
| A `ModelConfig` at argument position 0 | It has no model to attach to, so the whole config was dropped — a `ModelConfig{Headless: true}` silently did not apply and the model mounted routes you thought were suppressed. |
| Two `ModelConfig`s in a row | The second has no fresh model to bind to; same silent drop. |
| An invalid `mfx:"scheduled"` tag | The field was dropped from the sweep, so the scheduled transition you configured simply never ran. |
| `mfx:"relation"` on a field not ending in `ID` | The target model is derived by stripping that suffix. Without one it is inferred from the whole field name — almost never a real model. Write `mfx:"relation:Target"`. |
| A middleware registered on a step its operations never reach | It is frozen into the chain and never runs. When it is an authorisation check, that is a silent hole. |
| A `RequiresField` declaration no model can satisfy | The gate watches for a field name nothing has — see below. |

The first three are **registration errors**, returned from `Register` (so
`MustRegister` panics). The last two need the complete registry — a relation's
target may be registered after the model pointing at it — so they are raised
when the router is built.

## Declaring the fields a middleware gates

A middleware that gates a field by name has a nasty failure mode: misspell the
name and the gate watches for a body key nothing sends, while the real field
keeps its name and **nothing gates it**. It fails open, silently.

Nothing at runtime can detect this. From inside a request, "this model has no
such field" looks identical to a gate deliberately registered across models
where only some carry it. Only the registration knows which it is — so that is
where you say so:

```go
server.Pipeline.Validate.Register(
    validate.RestrictField("document_quota_bytes", isSuperuser),
    maniflex.ForModel("User"),
    maniflex.RequiresField("document_quota_bytes"), // ← checked at startup
)
```

Writing the name twice is not the tautology it looks like: the declared name is
checked against **the model**, not against the middleware's argument, so a
misspelling is caught either way.

The `ForModel` scope makes the check exact:

- **With `ForModel`** — every named model must have every declared field. The
  gate was aimed at those models specifically, so a model that lacks the field
  is a mistake.
- **Without it** — at least one registered model must have the field. A gate no
  model can trigger cannot be doing anything.

Use it for any middleware that gates or reads a field by name, not just
`validate.RestrictField` — `response.RedactField` and hand-written gates have
the same failure mode.

It is opt-in: a registration that declares nothing is not checked, so existing
code is unaffected. `validate.RestrictField` keeps its first-request warning as
a fallback for undeclared registrations, but that warning cannot fire for an
endpoint nobody exercises, and cannot tell a typo from a deliberate broad
registration. Declare the field.

## Gated by `Config.Strict`

```go
maniflex.New(maniflex.Config{Strict: true})
```

These stay warnings by default because each has a legitimate reading:

| Problem | The legitimate reading |
|---|---|
| `mfx:"relation"` whose target model is not registered | The field may be a plain foreign id that wants no relation tag. The FK column works either way. |
| The standalone `/files` endpoints mounted with no auth middleware | A deliberately public upload endpoint is conceivable, if rarely wise. |
| `Config.StaticDir` names a directory that does not exist | Static serving degrades to 404s. Failing the boot would let a missing frontend asset bundle take down a working API. |

**Turn it on in CI and staging**, where a boot failure costs a re-run rather
than an outage. Leave it off in production if you would rather serve a degraded
API than none.

## One report, not one restart at a time

Every problem found is listed together:

```
maniflex: 3 startup problems:
  1. [relation] Invoice.Owner is tagged mfx:"relation" but its name does not
     end in "ID", so the target model was inferred from the whole field name
     as "Owner" — write mfx:"relation:Target" to name the target explicitly
  2. [middleware] middleware "audit" is registered on the db step for
     operation(s) [search], all of which skip that step — it will never run;
     register it on a step those operations run, or widen ForOperation
  3. [static] Config.StaticDir "./pubic" does not exist, so nothing would be
     served under /static (Config.Strict)
```

Issues that are only fatal because strict mode is on are marked `(Config.Strict)`,
so you never hunt for a bug in configuration that is legal by default.

## When validation runs

Startup validation happens **before migrations and before services start**:

```
Start()
  ├── validate + build router   ← fails here
  ├── migrate
  ├── start services
  └── open listener
```

A configuration error therefore costs nothing to find — it cannot leave a
half-migrated schema behind.

`Start` and `StartWithContext` **return** the error. `Handler()` **panics**,
because it has no error return; if you mount maniflex inside another router,
that is the form you will see.

## What stays a warning

Some warnings describe a legitimate operational state and are not promoted even
under strict mode:

- **A column in the database that is not on the model.** Normal during a staged
  field removal; failing here would break rolling deploys.
- **A column whose type differs from the model's.** Promoting this would brick
  startup against an already-drifted production database.
- **Two scheduled fields targeting the same column** where the later omits
  `from=`. A real hazard, but a legal design.
