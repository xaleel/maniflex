# Example 2: B2B SaaS API

This example builds a small multi-tenant SaaS backend. Compared to
[Example 1](example-1.md), it exercises every concept introduced in the
"Defining Your API" and "The Request Pipeline" sections:

- Multiple related models with `BelongsTo` and `HasMany` relations.
- Soft delete on the records that matter for an audit trail.
- A bearer-token Auth middleware that populates `ctx.Auth`.
- A Service-step middleware that scopes every query to the caller's tenant.
- A Service-step middleware that runs inside a transaction.
- Custom error envelopes with `ctx.Abort`.

The goal is to show how the pieces compose; the auth and tenancy code is
deliberately minimal so the example fits on one page.

## Domain

A SaaS platform with three resources:

- **Organization** — the tenant. Every other record belongs to one.
- **Member** — a user belonging to an organization, with a role.
- **Project** — a unit of work owned by a member.

```go
type Organization struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    Name string `json:"name" mfx:"required,filterable,sortable"`
    Plan string `json:"plan" mfx:"required,enum:free|pro|enterprise,default:free"`

    Members  []Member  `json:"members,omitempty"`
    Projects []Project `json:"projects,omitempty"`
}

type Member struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    OrganizationID string `json:"organization_id" mfx:"required,filterable,immutable"`
    Email          string `json:"email"           mfx:"required,filterable,unique"`
    Role           string `json:"role"            mfx:"required,enum:owner|admin|editor|viewer,default:viewer,filterable"`

    Projects []Project `json:"projects,omitempty"`
}

type Project struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    OrganizationID string `json:"organization_id" mfx:"required,filterable,immutable"`
    OwnerID        string `json:"owner_id"        mfx:"required,filterable,relation:Owner"`
    Owner          Member `json:"owner,omitempty"`

    Name   string `json:"name"   mfx:"required,filterable,sortable"`
    Status string `json:"status" mfx:"required,enum:active|paused|archived,default:active,filterable,sortable"`
}
```

`Owner` is a companion field; the explicit `relation:Owner` tag is required
because the FK name (`OwnerID`) does not match the model name (`Member`).
`OrganizationID` follows the convention, so no companion is needed there.

## Wiring

A single `main.go` registers the models, installs three middlewares, and
starts the server.

```go
func main() {
    server := maniflex.New(maniflex.Config{
        Port:        8080,
        PathPrefix:  "/api",
        AutoMigrate: true,
    })

    server.MustRegister(Organization{}, Member{}, Project{})

    db, err := sqlite.Open("./saas.db", server.Registry())
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    server.SetDB(db)

    registerMiddleware(server)

    log.Fatal(server.Start())
}
```

## Auth — populating `ctx.Auth`

A real deployment would verify a JWT; this example resolves a bearer token
against an in-memory map to keep the focus on `ctx.Auth`:

```go
var tokens = map[string]maniflex.AuthInfo{
    "alice-token": {UserID: "user-alice", TenantID: "org-acme",  Roles: []string{"owner"}},
    "bob-token":   {UserID: "user-bob",   TenantID: "org-acme",  Roles: []string{"editor"}},
    "carol-token": {UserID: "user-carol", TenantID: "org-globex", Roles: []string{"admin"}},
}

func bearerAuth(ctx *maniflex.ServerContext, next func() error) error {
    header := ctx.Request.Header.Get("Authorization")
    token := strings.TrimPrefix(header, "Bearer ")
    info, ok := tokens[token]
    if !ok {
        ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing token")
        return nil
    }
    ctx.Auth = &info
    return next()
}
```

## Tenant scoping — filtering by `ctx.Auth.TenantID`

A B2B API must never leak data across tenants. A Service-step middleware
inspects every list and read, and rejects writes that would assign records to a
foreign organization:

```go
func enforceTenant(ctx *maniflex.ServerContext, next func() error) error {
    tenant := ctx.Auth.TenantID

    switch ctx.Operation {
    case maniflex.OpList:
        // Inject a filter so users only see their organization's rows.
        ctx.Query.Filters = append(ctx.Query.Filters, maniflex.Filter{
            Field: "organization_id", Op: "eq", Value: tenant,
        })

    case maniflex.OpCreate, maniflex.OpUpdate:
        if v, ok := ctx.Field("organization_id"); ok && v != tenant {
            ctx.Abort(http.StatusForbidden, "TENANT_MISMATCH",
                "organization_id does not match the authenticated tenant")
            return nil
        }
        ctx.SetField("organization_id", tenant)
    }
    return next()
}
```

The middleware applies to the two tenanted models — `Member` and `Project` —
and to every operation. `Organization` itself is not scoped, because the auth
middleware already binds each token to exactly one organization.

## Transactions — creating a project atomically

A new project should fail entirely if the member quota check fails. A Service
middleware wraps the operation in a transaction and uses
`ctx.LockForUpdate` to read the organization with a write lock:

```go
func enforceProjectQuota(ctx *maniflex.ServerContext, next func() error) error {
    org, err := ctx.LockForUpdate("Organization", ctx.Auth.TenantID)
    if err != nil {
        return err
    }

    rows, err := ctx.RawQuery(
        `SELECT COUNT(*) AS n FROM projects WHERE organization_id = ? AND deleted_at IS NULL`,
        ctx.Auth.TenantID,
    )
    if err != nil {
        return err
    }
    count := rows[0]["n"].(int64)

    limit := planLimit(org["plan"].(string))
    if count >= limit {
        ctx.Abort(http.StatusPaymentRequired, "PROJECT_LIMIT",
            fmt.Sprintf("plan %q allows %d projects; upgrade to add more", org["plan"], limit))
        return nil
    }
    return next()
}

func planLimit(plan string) int64 {
    switch plan {
    case "enterprise":
        return 1000
    case "pro":
        return 25
    default:
        return 3
    }
}
```

## Registering the middleware

All three middlewares are registered in one place:

```go
func registerMiddleware(s *maniflex.Server) {
    // Auth on every write — reads are public within the tenant once they
    // pass enforceTenant below; tighten or relax to taste.
    s.Pipeline.Auth.Register(bearerAuth,
        maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete, maniflex.OpList, maniflex.OpRead),
    )

    // Tenant scoping for the two tenanted models.
    s.Pipeline.Service.Register(enforceTenant,
        maniflex.ForModel("Member", "Project"),
    )

    // The DB step is wrapped in a transaction for project creation, and the
    // quota check runs inside it before next() reaches DB.
    s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
        maniflex.ForModel("Project"), maniflex.ForOperation(maniflex.OpCreate),
    )
    s.Pipeline.Service.Register(enforceProjectQuota,
        maniflex.ForModel("Project"), maniflex.ForOperation(maniflex.OpCreate),
    )
}
```

Order matters: `WithTransaction` is registered before `enforceProjectQuota`, so
the transaction is open by the time the quota check calls `ctx.LockForUpdate`.

## A request, end to end

```bash
# Alice (org-acme, owner) creates a project.
curl -X POST localhost:8080/api/projects \
  -H 'Authorization: Bearer alice-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"Atlas","owner_id":"user-alice"}'
```

What happens:

1. **Auth** — `bearerAuth` resolves `alice-token` and sets `ctx.Auth`.
2. **Deserialize** — JSON body parsed into `ctx.ParsedBody`.
3. **Validate** — `mfx:` tag rules pass; `organization_id` is missing but the
   next step injects it.
4. **Service**:
   1. `enforceTenant` writes `organization_id = "org-acme"` into the body.
   2. `WithTransaction` begins a transaction.
   3. `enforceProjectQuota` locks the organization row, counts existing
      projects, and either aborts with `402 PROJECT_LIMIT` or proceeds.
5. **DB** — `Create` runs through the transaction; `ctx.DBResult` holds the
   inserted row.
6. **Response** — the envelope is written; the transaction commits.

Carol's `carol-token` belongs to `org-globex`, so the same payload from her is
rejected before it reaches the DB step:

```bash
curl 'localhost:8080/api/projects?filter=organization_id:eq:org-acme' \
  -H 'Authorization: Bearer carol-token'
# → list filtered to org-globex only; the requested filter is ignored
```

## What this example showed

- Relations declared with both the convention (`OrganizationID`) and the
  explicit `relation:Owner` form.
- `WithDeletedAt` on every audited model.
- `ctx.Auth` populated by an Auth middleware, then read by Service middleware
  to scope queries.
- `ctx.Query.Filters` modified to enforce a tenant invariant.
- `maniflex.WithTransaction` plus `ctx.LockForUpdate` for a check-and-act write.
- Custom error codes (`TENANT_MISMATCH`, `PROJECT_LIMIT`) emitted with
  `ctx.Abort`.

## Where to go next

The next section covers the ready-made middleware that ships with maniflex — the
production-quality versions of the auth and validation helpers sketched here.

- **[Middleware Catalogue](middleware/index.md)** — JWT auth, password hashing,
  unique validation, audit logging, and more.
- **[Querying](querying.md)** — the full filter, sort, and `include` grammar
  used in this example.
