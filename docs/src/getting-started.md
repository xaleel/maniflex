# Getting Started

This guide takes you from an empty directory to a running REST API with a
filterable, paginated `posts` resource — in about five minutes and roughly
thirty lines of Go.

## Prerequisites

- **Go 1.25 or newer** — check with `go version`.
- That's it. The first example uses the pure-Go SQLite backend, so there is no
  database server to install and no CGo toolchain to configure.

## 1. Create the project

```bash
mkdir blog && cd blog
go mod init blog
go get github.com/xaleel/maniflex
```

`maniflex` itself pulls in only two dependencies — [chi](https://github.com/go-chi/chi)
and [uuid](https://github.com/google/uuid). The SQLite adapter lives in its own
satellite module, so add it explicitly:

```bash
go get github.com/xaleel/maniflex/db/sqlite
```

## 2. Define a model

A model is a plain struct that embeds `maniflex.BaseModel`. The `mfx:` struct tags
declare how each field behaves — what's required, what can be filtered, what
can be sorted.

Create `main.go`:

```go
package main

import (
    "log"

    "github.com/xaleel/maniflex"
    "github.com/xaleel/maniflex/db/sqlite"
)

type Post struct {
    maniflex.BaseModel
    Title  string `json:"title"  mfx:"required,filterable,sortable"`
    Body   string `json:"body"   mfx:"required"`
    Status string `json:"status" mfx:"required,filterable,enum:draft|published|archived"`
}
```

`maniflex.BaseModel` contributes the `id`, `created_at`, and `updated_at` fields, so
you never declare them yourself. See [Field Tags Reference](defining-your-api/tags.md) for every
tag and [Models & BaseModel](defining-your-api/models.md) for the embeds.

## 3. Wire up the server

Registration order matters: **models must be registered before the database is
opened**, because the SQLite adapter needs the registry to run migrations and
resolve relations.

```go
func main() {
    // 1. Create the server — no DB yet.
    server := maniflex.New(maniflex.Config{
        Port:        8080,
        PathPrefix:  "/api",
    })

    // 2. Register models — this populates the registry.
    server.MustRegister(Post{})

    // 3. Open SQLite with the populated registry.
    db, err := sqlite.Open("./blog.db", server.Registry())
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // 4. Inject the adapter into the pipeline.
    server.SetDB(db)

    // 5. Serve.
    log.Fatal(server.Start())
}
```

Migration (on by default) creates and updates tables to match your structs on
startup — convenient for development. For an in-memory database that resets on
every run, pass `":memory:"` to `sqlite.Open`.

## 4. Run it

```bash
go run .
```

The server is now listening on `:8080`, and `Post{}` has a full set of routes
mounted under the `/api` prefix:

| Method   | Path              | Action        |
| -------- | ----------------- | ------------- |
| `POST`   | `/api/posts`      | create a post |
| `GET`    | `/api/posts`      | list posts    |
| `GET`    | `/api/posts/{id}` | read one post |
| `PATCH`  | `/api/posts/{id}` | update a post |
| `DELETE` | `/api/posts/{id}` | delete a post |

## 5. Make some requests

Create a post:

```bash
curl -X POST localhost:8080/api/posts \
  -H 'Content-Type: application/json' \
  -d '{"title":"Hello","body":"First post","status":"published"}'
```

List, filter, sort, and paginate — all from the query string:

```bash
# Only published posts
curl 'localhost:8080/api/posts?filter=status:eq:published'

# Newest first, ten per page
curl 'localhost:8080/api/posts?sort=created_at:desc&page=1&limit=10'
```

Filtering and sorting only work on fields tagged `filterable` / `sortable` —
that's why `Title` and `Status` carry those tags above. The full filter grammar
is in [Querying](using-the-api/querying.md).

## What you get for free

From that one struct, maniflex derived:

- Five REST endpoints with JSON request/response handling.
- Field validation (`required`, `enum`) on every write.
- Query-string filtering, sorting, and pagination.
- A generated table kept in sync by `AutoMigrate`.
- An OpenAPI 3.1 entry at `/api/openapi.json`.

No generated code, no per-endpoint handlers.

## Where to go next

- **[Quickstart Tutorial](tutorial.md)** — build a small app end to end.
- **[Models & BaseModel](defining-your-api/models.md)** — relations, soft-delete, file fields.
- **[The Request Pipeline](the-request-pipeline/pipeline.md)** — the six steps every request flows
  through, and where to hook in your own middleware.
- **[Querying](using-the-api/querying.md)** — the full filter, sort, and `include` grammar.
- **[Database Backends](deployment/databases.md)** — switching from SQLite to PostgreSQL.
