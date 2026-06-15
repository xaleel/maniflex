# Example 1: Simple Blog

This is the first worked example: a small blog API built end to end. It uses
only what the previous pages covered — [models and `mfx:` tags](getting-started.md),
the four-step setup, and SQLite. No relations, no middleware, no pipeline
customisation yet; those arrive in later chapters. The goal is to see a complete,
runnable app with nothing unexplained in it.

## What we're building

A blog with two independent resources:

- **Post** — an article with a title, body, and a publication status.
- **Subscriber** — an email address signed up for the newsletter.

The two are unrelated — each is its own table with its own endpoints — which
keeps the example to concepts already introduced.

## The whole app

The blog is small enough to live in a single `main.go`, the
[smallest-app shape from App Anatomy](anatomy.md):

```go
package main

import (
    "log"

    "maniflex"
    "maniflex/db/sqlite"
)

// Post is a blog article.
type Post struct {
    maniflex.BaseModel
    Title  string `json:"title"  mfx:"required,filterable,sortable"`
    Body   string `json:"body"   mfx:"required"`
    Status string `json:"status" mfx:"required,filterable,sortable,enum:draft|published|archived"`
}

// Subscriber is a newsletter sign-up.
type Subscriber struct {
    maniflex.BaseModel
    Email string `json:"email" mfx:"required,filterable"`
    Name  string `json:"name"  mfx:"filterable,sortable"`
}

func main() {
    // 1. Create the server.
    server := maniflex.New(maniflex.Config{
        Port:        8080,
        PathPrefix:  "/api",
        AutoMigrate: true,
    })

    // 2. Register both models — populates the registry.
    server.MustRegister(Post{}, Subscriber{})

    // 3. Open SQLite with the populated registry, then inject it.
    db, err := sqlite.Open("./blog.db", server.Registry())
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    server.SetDB(db)

    // 4. Serve.
    log.Fatal(server.Start())
}
```

That is the entire blog. Run it:

```bash
go run .
```

## Reading the models

Every field choice maps to a tag covered in [Getting Started](getting-started.md):

| Field | Tags | Effect |
|---|---|---|
| `Title` | `required,filterable,sortable` | must be present; usable in `?filter=` and `?sort=` |
| `Body` | `required` | must be present; not queryable |
| `Status` | `required,...,enum:draft\|published\|archived` | rejected unless one of the three values |
| `Email` | `required,filterable` | must be present; filterable but not sortable |
| `Name` | `filterable,sortable` | optional; queryable and sortable |

`maniflex.BaseModel` adds `id`, `created_at`, and `updated_at` to both structs, so
those are never declared by hand. With `AutoMigrate: true`, the `posts` and
`subscribers` tables are created to match on startup.

## The endpoints you get

Registering the two structs mounts a full REST surface under `/api`:

| Method | Path | |
|---|---|---|
| `POST` | `/api/posts` | create a post |
| `GET` | `/api/posts` | list posts |
| `GET` | `/api/posts/{id}` | read one post |
| `PATCH` | `/api/posts/{id}` | update a post |
| `DELETE` | `/api/posts/{id}` | delete a post |

`Subscriber` gets the identical five routes under `/api/subscribers`.

## Using the API

### Create a post

```bash
curl -X POST localhost:8080/api/posts \
  -H 'Content-Type: application/json' \
  -d '{"title":"Hello World","body":"My first post","status":"draft"}'
```

The response echoes the stored row, including the `id` and timestamps that
`BaseModel` filled in.

### Validation in action

Leave out a `required` field, or send a `status` outside the enum, and the
write is rejected before it reaches the database:

```bash
# Missing body, and status is not in the enum
curl -X POST localhost:8080/api/posts \
  -H 'Content-Type: application/json' \
  -d '{"title":"Broken","status":"weekly"}'
# → 400, the response names the offending fields
```

### Update and delete

```bash
# Publish the post — PATCH only sends the fields that change
curl -X PATCH localhost:8080/api/posts/<id> \
  -H 'Content-Type: application/json' \
  -d '{"status":"published"}'

curl -X DELETE localhost:8080/api/posts/<id>
```

### List, filter, sort, paginate

All from the query string, on the fields tagged `filterable` / `sortable`:

```bash
# Only published posts
curl 'localhost:8080/api/posts?filter=status:eq:published'

# Newest first
curl 'localhost:8080/api/posts?sort=created_at:desc'

# Page two, five per page
curl 'localhost:8080/api/posts?page=2&limit=5'

# Combine them
curl 'localhost:8080/api/posts?filter=status:eq:published&sort=title:asc&limit=5'
```

Add a couple of subscribers and the same querying works there too:

```bash
curl -X POST localhost:8080/api/subscribers \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","name":"Ada"}'

curl 'localhost:8080/api/subscribers?sort=name:asc'
```

## The API documents itself

You never wrote a schema, yet the server already publishes one. Alongside the
model routes, maniflex auto-generates an **OpenAPI 3.1** specification describing
every endpoint, field, and validation rule — derived from the same structs and
`mfx:` tags:

```bash
curl localhost:8080/api/openapi.json
```

The spec updates itself whenever a model changes; there is nothing to
regenerate. To browse it as interactive documentation, the framework ships an
HTML viewer in `static/openapi.html` that loads `/api/openapi.json` — open
<http://localhost:8080/static/openapi.html> while the server is running.

The OpenAPI step is fully customisable later; see [OpenAPI Spec](openapi.md).

## What this example showed

- A complete, runnable app from two plain structs and a four-step `main`.
- `mfx:` tags driving validation (`required`, `enum`) and queryability
  (`filterable`, `sortable`).
- Multiple models registered in one call, each with an independent REST surface.
- Filtering, sorting, and pagination with no query code written.
- A self-updating OpenAPI 3.1 spec at `/api/openapi.json`.

## Where to go next

Everything here treated the two models as separate islands. Real apps connect
them — a post *belongs to* an author, a comment *belongs to* a post. That, and
the `mfx:` tags beyond the basics, are the next chapters:

- **[Models & BaseModel](models.md)** — the embeds and what they contribute.
- **[Field Tags Reference](tags.md)** — every `mfx:` tag.
- **[Relations](relations.md)** — connecting models with foreign keys.
