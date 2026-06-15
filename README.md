# maniflex

_manifold + flexible_ - many shapes from one flexible core.

A Go framework that turns annotated structs into a complete REST API: filtering,
pagination, relations, soft-delete, and a composable middleware pipeline, all derived
at runtime by reflection. No code generation.

Define a struct, register it, point it at a database. You get CRUD routes, query
parsing, validation, an OpenAPI spec, and relation loading for free.

## Features

- Full CRUD with keyset pagination, filtering, sorting, and field projection
- Relations by convention (`BelongsTo`, `HasMany`) populated via `?include=`
- Soft-delete, history versioning, and field-level encryption
- A six-step pipeline (Auth, Deserialize, Validate, Service, DB, Response) with `Before`/`After`/`Replace` middleware
- OpenAPI 3.1 and AsyncAPI 2.6 spec generation
- Event bus, background jobs, WebSocket/SSE, and an admin panel
- Pluggable backends: PostgreSQL and pure-Go SQLite (no CGo)
- Minimal core: only chi and uuid; heavy integrations live in opt-in satellite modules

## Install

```bash
go get github.com/xaleel/maniflex
go get github.com/xaleel/maniflex/db/sqlite
```

## Quickstart

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

func main() {
	server := maniflex.New(maniflex.Config{
		Port:        8080,
		PathPrefix:  "/api",
		AutoMigrate: true,
	})

	// Register models before opening the DB - the adapter needs the registry
	// to run migrations and resolve relations.
	server.MustRegister(Post{})

	db, err := sqlite.Open("./blog.db", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	server.SetDB(db)

	log.Fatal(server.Start())
}
```

`Post{}` now has a full set of routes under `/api`:

```bash
curl -X POST localhost:8080/api/posts -d '{"title":"Hello","body":"...","status":"draft"}'
curl 'localhost:8080/api/posts?filter=status:eq:published&sort=created_at:desc&page=1&limit=10'
```

## Documentation

Full documentation, guides, and the tutorial series are at **[maniflex.dev](https://maniflex.dev)**.

## Modules

`maniflex` is a multi-module monorepo. The core carries only chi and uuid; each
satellite module isolates one heavy dependency so you pull only what you import -
`db/postgres`, `db/sqlite`, `events/{kafka,nats,rabbitmq,redis}`, `jobs/redis`,
`middleware/service/bcrypt`, `storage/s3`, `pkg/otel`, and more.

## Requirements

Go 1.25 or newer.

## License

MIT
