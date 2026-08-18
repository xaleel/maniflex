# maniflex

[![CI](https://github.com/xaleel/maniflex/actions/workflows/ci.yml/badge.svg)](https://github.com/xaleel/maniflex/actions/workflows/ci.yml)

_manifold + flexible_ - many shapes from one flexible core.

A Go framework that turns annotated structs into a complete REST API: filtering,
pagination, relations, soft-delete, and a composable middleware pipeline, all derived
at runtime by reflection. No code generation.

Define a struct, register it, point it at a database. You get CRUD routes, query
parsing, validation, relation loading, and a generated OpenAPI spec for free.

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
		Port:       8080,
		PathPrefix: "/api",
		// The spec is always generated, this config enables serving it
		Documentation: maniflex.DocumentationConfig{Public: true},
	})

	// Register models before opening the DB - the adapter needs the registry
	// to run migrations and resolve relations.
	server.MustRegister(Post{}, maniflex.ModelConfig{
		BaseModelTags: map[string]string{"created_at": "filterable,sortable"},
	})

	db, err := sqlite.Open("./blog.db", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	server.SetDB(db)

	log.Fatal(server.Start())
}
```

`Start` creates the table for each registered model; set `DisableAutoMigrate:
true` when migrations are managed out of band. This example is compiled as
[`examples/quickstart`](examples/quickstart/main.go) — edit the two together.

`Post{}` now has a full set of routes under `/api`:

```bash
curl -X POST localhost:8080/api/posts -d '{"title":"Hello","body":"...","status":"draft"}'
curl 'localhost:8080/api/posts?filter=status:eq:published&sort=created_at:desc&page=1&limit=10'
curl localhost:8080/api/openapi.json
```

## Documentation

Full documentation, guides, and the tutorial series are at **[maniflex.dev](https://docs.maniflex.dev)**.

## Modules

`maniflex` is a multi-module monorepo. The core carries only chi and uuid; each
satellite module isolates one heavy dependency so you pull only what you import -
`db/postgres`, `db/sqlite`, `events/{kafka,nats,rabbitmq,redis}`, `jobs/redis`,
`middleware/service/bcrypt`, `storage/s3`, `pkg/otel`, and more.

## Stability

maniflex is v0.x: any minor release may break. The compatibility contract that
takes effect at v1.0.0 — what is covered, how deprecation works, and which
modules are held back — is documented in
[Stability & Compatibility](https://docs.maniflex.dev/reference/compatibility.html).

## Requirements

Go 1.25.12 or newer.

## License

MIT
