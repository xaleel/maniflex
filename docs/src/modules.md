# Satellite Modules

`maniflex` is a multi-module monorepo. The core module — also named `maniflex` —
carries only chi and uuid. Every heavy dependency (a database driver, a
message broker client, a crypto library) lives in its own _satellite_ module
under the same repository, so a consumer pulls only the dependencies it
imports.

## Layout

```
maniflex/                       # core module — chi + uuid
├── maniflex/               # framework
├── storage/                 # local disk file storage
└── ...

storage/
└── s3/                      # aws-sdk-go-v2 — S3, MinIO, R2, Spaces, etc.

db/
├── sqlite/                  # modernc.org/sqlite — pure-Go SQLite
├── postgres/                # lib/pq — PostgreSQL
└── sqlcore/                 # shared SQL adapter used by both

events/
├── kafka/                   # confluent-kafka-go
├── nats/                    # nats.go
├── rabbitmq/                # streadway/amqp
└── redis/                   # go-redis

jobs/
├── inproc/                  # goroutine pool — tests and single-binary apps
├── sql/                     # *sql.DB-backed (Postgres + SQLite) with transactional outbox
├── redis/                   # Redis Streams / BRPOP — high-throughput worker fleets
├── cron/                    # scheduled EnqueueAt ticker
└── maniflex/                # StatusModel + Mount helper — REST polling for job status

middleware/
├── auth/, body/, db/, …     # catalogue middleware (see Middleware Catalogue)
├── service/bcrypt/          # golang.org/x/crypto for password hashing
└── db/redis/                # Redis cache invalidation

examples/                    # runnable example apps — its own module
tests/                       # e2e suite — its own module
```

## Why split modules

The split keeps the core dependency graph minimal:

- A project that uses SQLite imports `maniflex/db/sqlite` and gets the pure-Go
  driver. PostgreSQL's `lib/pq` is **not** in its build.
- A project that publishes events to Kafka imports `maniflex/events/kafka` and
  pulls in `confluent-kafka-go`. NATS and RabbitMQ stay out of the build.
- A project that does not authenticate doesn't import `middleware/auth` and
  pays nothing for the JWT library.

This matters most for binary size, attack surface, and CI build time. It also
keeps the core stable: changing a database driver does not require a release
of the framework itself.

## Importing satellites

Each satellite is a normal Go module — add it with `go get`:

```bash
go get github.com/xaleel/maniflex                 # core
go get github.com/xaleel/maniflex/db/sqlite       # SQLite adapter
go get github.com/xaleel/maniflex/middleware/auth # auth helpers
go get github.com/xaleel/maniflex/events/kafka    # Kafka publisher
```

In code:

```go
import (
    "maniflex"
    "maniflex/db/sqlite"
    "maniflex/middleware/auth"
    "maniflex/events/kafka"
)
```

There are no required satellites for the framework itself to function —
`maniflex` alone gives you the registry, pipeline, and HTTP layer. You need
_at least_ a database adapter (sqlite or postgres) before `server.Start()` can
serve a request.

## Workspace mode

The repository ships a `go.work` file that includes every satellite module.
For consumers, this is invisible — Go modules resolve normally through
`go.mod`. For contributors working across modules, `go.work` makes
cross-module changes possible without `replace` directives.

`go build ./...` and `go test ./...` operate per-module. To build or test
every module at once, use the helper scripts in [scripts/](scripts/):

```bash
bash scripts/test-all.sh
# or
powershell scripts/test-all.ps1
```

## Versioning

Each satellite carries its own `v0.x` tags. The core module is the only one a
typical app depends on by name; satellites are usually pulled in transitively
or by direct import as needed. Pin satellite versions in `go.mod` when
reproducibility across machines is required.
