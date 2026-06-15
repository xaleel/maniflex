# App Anatomy

A maniflex app has very little machinery of its own — the framework derives routes,
schema, and validation from your structs, so the code you write is mostly
_models_ and _middleware_. This page shows how to lay that code out, starting
from a single file and growing into packages as the app gets bigger.

## The smallest app

A maniflex app is just a `package main` that does four things in order:

```go
package main

import (
    "log"

    "maniflex"
    "maniflex/db/sqlite"
)

type Message struct {
    maniflex.BaseModel
    Title string `json:"title" mfx:"required,filterable,sortable"`
}

func main() {
    server := maniflex.New(maniflex.Config{Port: 8080, PathPrefix: "/api", AutoMigrate: true})
    server.MustRegister(Message{})

    if db, err := sqlite.Open("./app.db", server.Registry()); err == nil {
        defer db.Close()
        server.SetDB(db)
    } else {
        log.Fatal(err)
    }

    log.Fatal(server.Start())
}
```

Four steps — **create server → register models → open and set DB → serve**.
Everything below is about _where the code for each step lives_ as the app grows.
The ordering is load-bearing: `MustRegister` must run before `sqlite.Open`,
because the adapter is handed the populated registry. See
[Getting Started](getting-started.md).

## A typical project layout

Once an app has more than a handful of models, split it into packages by
_responsibility_, not by model. A layout that scales well:

```
myapp/
├── go.mod
├── main.go               # wiring only: create, register, set DB, serve
├── config.go             # maniflex.Config assembly, env-var reading
├── models/               # one file per model — the structs and their tags
│   ├── user.go
│   ├── post.go
│   └── comment.go
├── middleware/            # custom pipeline middleware
│   ├── auth.go
│   ├── audit.go
│   └── register.go        # attaches all middleware to the pipeline
└── internal/              # non-framework code: services, clients, helpers
    └── mailer/
    └── ...
```

Nothing here is enforced by the framework — maniflex never scans directories. It is
a convention that keeps each file answering one question.

## What goes in each file

### `main.go` — wiring, nothing else

`main.go` should read top-to-bottom as the four-step sequence and contain no
business logic. Its whole job is to assemble the pieces and call `Start()`:

```go
func main() {
    server := maniflex.New(config.Load())

    server.MustRegister(
        models.User{},
        models.Post{},
        models.Comment{},
    )

    db, err := sqlite.Open("./app.db", server.Registry())
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    server.SetDB(db)

    middleware.Register(server)   // all pipeline hooks, in one place

    log.Fatal(server.Start())
}
```

If you can't see the four steps at a glance, something belongs in another file.

### `config.go` — building `maniflex.Config`

Keep `maniflex.Config` construction — and any environment-variable reading — out of
`main.go`. A single `Load()` function makes the app's knobs easy to find:

```go
package config

func Load() maniflex.Config {
    return maniflex.Config{
        Port:        envInt("PORT", 8080),
        PathPrefix:  "/api",
        AutoMigrate: env("APP_ENV", "dev") != "production",
    }
}
```

See [Configuration](config.md) for every `maniflex.Config` field.

### `models/` — one file per model

Each file holds one struct, its `mfx:` tags, and the relation fields that point
at _other_ models. This is the heart of the app — the struct _is_ the table, the
JSON shape, and the validation rules all at once.

```go
// models/post.go
package models

type Post struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt                       // opt-in soft-delete
    Title  string `json:"title"  mfx:"required,filterable,sortable"`
    Body   string `json:"body"   mfx:"required"`
    Status string `json:"status" mfx:"required,filterable,enum:draft|published|archived"`

    UserID   string    `json:"user_id"  mfx:"required,filterable"` // BelongsTo User
    Comments []Comment `json:"comments,omitempty"`                 // HasMany
}
```

Put model-spanning relations in whichever file is the "owning" side and let Go's
package scope resolve the rest — all models share the `models` package, so
`Post` can reference `Comment` freely. See [Models & BaseModel](models.md),
[Field Tags Reference](tags.md), and [Relations](relations.md).

### `middleware/` — your pipeline hooks

A middleware is a `func(ctx *maniflex.ServerContext, next func() error) error`. Group
related hooks per file (`auth.go`, `audit.go`), and keep one `register.go` that
attaches them all — so there is exactly one place to see how the request
pipeline has been customised:

```go
// middleware/register.go
package middleware

func Register(s *maniflex.Server) {
    s.Pipeline.Auth.Register(bearerToken,
        maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete))

    s.Pipeline.Service.Register(hashPassword,
        maniflex.ForModel("User"),
        maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))

    s.Pipeline.DB.Register(auditLog,
        maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
        maniflex.AtPosition(maniflex.After))
}
```

The middleware functions themselves live in their topic files. See
[Writing Middleware](middleware.md) and the [Middleware Catalogue](middleware/index.md)
for hooks that already ship with the framework.

### `internal/` — everything that isn't maniflex

Code that has nothing to do with the framework — a mail client, a payment SDK
wrapper, domain calculations — goes under `internal/`. Middleware in the
`Service` step calls into these packages; the packages themselves never import
`maniflex`. This keeps the framework-facing layer thin and your business logic
unit-testable on its own.

## Structuring a large monolith

The layer-based layout above (`models/`, `middleware/`) stays readable up to a
few dozen models. Past that, a flat `models/` directory with sixty files and a
`main.go` that names every one of them becomes the bottleneck. Large maniflex
codebases switch from splitting by _layer_ to splitting by _domain_.

### Domain packages

Give each business domain its own package that owns _all_ of its code — models,
middleware, and business logic together:

```
myapp/
├── main.go
├── domains/
│   ├── auth/
│   │   ├── models.go        # User, Role, Session, ApiKey
│   │   ├── middleware.go    # token auth, password hashing
│   │   └── register.go      # exports Models and Register(s)
│   ├── catalog/
│   │   ├── models.go        # Product, Category, Variant
│   │   ├── middleware.go
│   │   └── register.go
│   └── orders/
│       ├── models.go        # Order, LineItem, Invoice, Refund
│       ├── middleware.go
│       └── register.go
└── internal/
```

A new feature now touches one directory instead of being smeared across
`models/`, `middleware/`, and `internal/`.

### Registering models in groups

`server.Register` (and `MustRegister`) is variadic and **flattens any slice
argument** — so each domain can export its models as a single slice, and
`main.go` registers the slices side by side:

```go
// domains/auth/register.go
package auth

// Models is every model this domain owns. One list, one place to update.
var Models = []any{
    User{},
    Role{},
    Session{},
    ApiKey{},
}
```

```go
// main.go
server.MustRegister(
    auth.Models,      // each argument is a []any —
    catalog.Models,   // Register flattens them into individual models
    orders.Models,
)
```

`main.go` no longer grows when a domain gains a model; only that domain's
`Models` slice changes. To register everything as one list instead, concatenate
the slices: `append(append(auth.Models, catalog.Models...), orders.Models...)`.

### Registering middleware in groups

Apply the same idea to the pipeline. Each domain exposes its own
`Register(s *maniflex.Server)`, and the top-level middleware registration just calls
each one — exactly the shape in the request from a growing app:

```go
// domains/orders/register.go
package orders

// Register attaches every pipeline hook this domain needs.
func Register(s *maniflex.Server) {
    s.Pipeline.Validate.Register(checkStock, maniflex.ForModel("Order"))
    s.Pipeline.Service.Register(chargePayment,
        maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpCreate))
    s.Pipeline.DB.Register(emitOrderEvent,
        maniflex.ForModel("Order"), maniflex.AtPosition(maniflex.After))
}
```

```go
// main.go (or a thin top-level middleware/register.go)
func registerMiddleware(s *maniflex.Server) {
    auth.Register(s)
    catalog.Register(s)
    orders.Register(s)
}
```

Each domain controls its own pipeline hooks; the top-level function is just a
table of contents.

### Co-locating per-model middleware

For middleware that belongs to exactly one model, skip the separate `Register`
call entirely — `ModelConfig.Middleware` lets you attach hooks at registration
time, scoped to that model automatically:

```go
server.MustRegister(
    Order{}, maniflex.ModelConfig{
        Middleware: &maniflex.ModelMiddleware{
            Validate: []maniflex.MiddlewareFunc{checkStock},
            Service:  []maniflex.MiddlewareFunc{chargePayment},
        },
    },
)
```

This keeps a model and its rules in one declaration — useful when the hook is
meaningless without the model.

### Conventions that keep a big monolith honest

- **One registration point per concern.** Exactly one place lists the model
  groups, and one lists the middleware groups. If you can't find where a model
  is registered, the structure has drifted.
- **Domains depend inward, never sideways.** A domain may import `internal/` and
  `maniflex`; it should not import a sibling domain. Cross-domain relations are
  expressed by FK fields (a string `UserID`), which need no import.
- **`internal/` holds framework-free logic.** Payment, mail, and pricing code
  lives here and never imports `maniflex`, so it stays unit-testable in isolation.
- **`main.go` stays a fixed size.** Adding a domain adds one line to each
  registration list and nothing else. If `main.go` grows with the app, logic
  has leaked into it.

## How a request moves through the files

Tracing one `POST /api/posts` shows why the layout is split this way:

1. The router (built from the **registry** in `main.go`) matches the route.
2. The request enters the **pipeline** — `Auth → Deserialize → Validate →
Service → DB → Response`.
3. At each step, the hooks from `middleware/register.go` run, scoped by model
   and operation.
4. `Validate` checks the `mfx:` tags declared in `models/post.go`.
5. `Service` middleware may call into `internal/` for business logic.
6. The **adapter** injected via `SetDB` runs the SQL at the `DB` step.

Each file owns one stage of that journey — which is exactly why a growing app
stays readable.

## Where to go next

- **[Example 1: Simple Blog](example-1.md)** — this layout filled in for a real
  three-model app.
- **[The Request Pipeline](pipeline.md)** — the six steps in depth.
- **[Configuration](config.md)** — every `maniflex.Config` field.
