# 1. Overview & Scaffolding

This is the start of a ten-part walkthrough. The end product is a small but
realistic bookstore API — users sign in, browse books and reviews, place
orders, and the system notifies them when an order ships. Each step
introduces one capability of the framework; nothing is hand-waved.

Where the reference pages describe a feature in isolation, the tutorial
shows how the features compose in a real application.

## What you'll build

`bookstore` — an HTTP API with:

| Endpoint family | Capability |
|---|---|
| `/api/users` | sign-up, JWT auth, role-based access |
| `/api/books`, `/api/authors`, `/api/genres` | the catalogue, with relations and full querying |
| `/api/reviews` | per-book ratings with custom validation |
| Book cover upload via `multipart/form-data` | a `file` field |
| `POST /api/orders/place` | a transactional action with stock locking |
| Outbox + background worker | email a receipt after each order |

By the end of part 10 the same code base will be deployed to production with
PostgreSQL, env-driven configuration, and a health probe.

## Prerequisites

- **Go 1.25 or newer**.
- A text editor and a terminal. Nothing else; the development database is
  pure-Go SQLite, no CGo needed.

## Project layout

The app follows the layer-based layout described in
[App Anatomy](../anatomy.md). It grows over the tutorial; this is the shape
at the end:

```
bookstore/
├── go.mod
├── main.go                  # wiring: create, register, set DB, serve
├── config.go                # maniflex.Config assembly
├── models/                  # one file per model
│   ├── user.go
│   ├── book.go
│   ├── author.go
│   ├── genre.go
│   ├── review.go
│   ├── order.go
│   └── outbox.go
├── middleware/              # custom middleware
│   ├── auth.go
│   ├── validate.go
│   └── register.go
├── actions/                 # custom endpoints
│   └── orders.go
├── jobs/                    # background workers
│   └── outbox.go
└── static/
    └── openapi.html         # bundled API viewer
```

## Bootstrap

Create the directory, initialise a module, and add the framework:

```bash
mkdir bookstore && cd bookstore
go mod init bookstore
go get maniflex maniflex/db/sqlite
```

`main.go` starts as the smallest maniflex app:

```go
package main

import (
    "log"

    "maniflex"
    "maniflex/db/sqlite"
)

func main() {
    server := maniflex.New(maniflex.Config{
        Port:        8080,
        PathPrefix:  "/api",
        AutoMigrate: true,
    })

    db, err := sqlite.Open("./bookstore.db", server.Registry())
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    server.SetDB(db)

    log.Fatal(server.Start())
}
```

Run it:

```bash
go run .
```

The server starts on `:8080`. Nothing is registered yet, so the only endpoint
that responds is `/health` — but `GET /api/openapi.json` already serves a
valid (empty) OpenAPI document.

## Why the four-step shape

The framework's lifecycle never deviates from the same four steps —
**create → register → set DB → serve**. Everything we add in the next nine
parts plugs into one of those steps:

- **Create**: `maniflex.Config` grows with logger, file storage, query timeout,
  and so on.
- **Register**: more models, each carrying their `mfx:` tags.
- **Set DB**: SQLite for development, PostgreSQL by part 10.
- **Serve**: more pipeline middleware, but `Start()` itself stays the same.

The structure of `main.go` will not change between this part and part 10. The
file simply grows new lines.

## Next

In **[Part 2 — Users & Auth](2-auth.md)** we add the `User` model, a
sign-up endpoint, and JWT-based authentication. By the end of part 2 every
write request will require a valid token.
