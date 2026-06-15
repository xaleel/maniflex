# 2. Users & Auth

We start with the `User` model and the auth layer. By the end of this part,
the API has a sign-up endpoint, password hashing, JWT-based authentication on
all writes, and a role-based admin gate on user deletion.

## The model

Create `models/user.go`:

```go
package models

import "maniflex"

type User struct {
    maniflex.BaseModel

    Email    string `json:"email"    mfx:"required,filterable,unique,immutable"`
    Password string `json:"password" mfx:"required,writeonly,min:8"`
    Name     string `json:"name"     mfx:"required,filterable,sortable"`
    Role     string `json:"role"     mfx:"required,enum:admin|customer,default:customer,filterable"`
}
```

A few tag choices to notice:

- **`email`** is `unique` and `immutable` — once a user signs up, the address
  is the account identity.
- **`password`** is `writeonly` so it is accepted on input but never appears
  in responses, and `min:8` enforces a minimum length.
- **`role`** is an enum with a safe default; we'll gate `admin` writes
  separately in middleware.

Register it from `main.go`:

```go
import "bookstore/models"

server.MustRegister(models.User{})
```

That alone gives you `POST /api/users` (sign-up), `GET /api/users/{id}`,
`PATCH /api/users/{id}`, `DELETE /api/users/{id}`, and `GET /api/users`. But
right now anyone can call any of them — we need to hash passwords on the way
in and gate the writes.

## Hashing passwords

Add `maniflex/middleware/service/bcrypt`:

```bash
go get github.com/xaleel/maniflex/middleware/service/bcrypt
```

Then register the hashing middleware on the Service step, scoped to `User`
create and update:

```go
import "maniflex/middleware/service"

server.Pipeline.Service.Register(
    service.HashField("password"),
    maniflex.ForModel("User"),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
)
```

The middleware reads the `password` field (`ctx.Field`), replaces it with the
bcrypt hash via `ctx.SetField`, and lets the DB step write the hash. Nothing else in the application
needs to know that the column is hashed.

## JWT authentication

Pull in `maniflex/middleware/auth`:

```bash
go get github.com/xaleel/maniflex/middleware/auth
```

Register `JWTAuth` on the Auth step, scoped to writes — we'll let reads stay
public for now:

```go
import "maniflex/middleware/auth"

server.Pipeline.Auth.Register(
    auth.JWTAuth("dev-secret", auth.JWTOptions{Issuer: "bookstore"}),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
)
```

`JWTAuth` verifies the `Authorization: Bearer <token>` header, parses the
claims, and populates `ctx.Auth` with the user ID and roles. Tokens fail with
`401 UNAUTHORIZED`; missing tokens fail the same way.

Sign-up (`POST /api/users`) is itself a write — and a write that should
_not_ require a token, since the user does not exist yet. Add an exception:

```go
server.Pipeline.Auth.Register(
    auth.AllowPublicWrite(),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
)
```

`AllowPublicWrite` returns immediately for matching requests, bypassing the
JWT check. Registering it before `JWTAuth` (which we did) means it runs
first.

## Role-gated deletes

Only admins should be able to delete users. `auth.RequireRole` does exactly
that:

```go
server.Pipeline.Auth.Register(
    auth.RequireRole("admin"),
    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpDelete),
)
```

It runs after `JWTAuth`, so by the time it fires `ctx.Auth.Roles` is
populated. Non-admin users receive `403 FORBIDDEN`.

## Issuing tokens

`JWTAuth` only verifies tokens — it does not issue them. For development we
add a tiny token endpoint as a [custom action](../advanced/actions.md):

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/auth/login",
    Handler: login,
})

func login(ctx *maniflex.ServerContext) error {
    var req struct {
        Email    string `json:"email"`
        Password string `json:"password"`
    }
    if err := ctx.BindJSON(&req); err != nil {
        return nil
    }

    rows, err := ctx.RawQuery(
        `SELECT id, password, role FROM users WHERE email = ?`, req.Email,
    )
    if err != nil || len(rows) == 0 {
        ctx.Abort(http.StatusUnauthorized, "INVALID_CREDENTIALS", "bad email or password")
        return nil
    }
    user := rows[0]
    if !checkBcrypt(user["password"].(string), req.Password) {
        ctx.Abort(http.StatusUnauthorized, "INVALID_CREDENTIALS", "bad email or password")
        return nil
    }

    token := signJWT("dev-secret", user["id"].(string), []string{user["role"].(string)})
    ctx.Response = &maniflex.APIResponse{
        StatusCode: http.StatusOK,
        Data:       map[string]any{"token": token},
    }
    return nil
}
```

`signJWT` and `checkBcrypt` are small helpers built on `github.com/golang-jwt/jwt/v5`
and `maniflex/middleware/service/bcrypt`. In production this endpoint would
issue a refresh token too — for now, a single bearer token is enough.

## Trying it out

```bash
# Sign up
curl -X POST localhost:8080/api/users \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"hunter22!","name":"Alice"}'

# Log in
TOKEN=$(curl -s -X POST localhost:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"hunter22!"}' \
  | jq -r .data.token)

# Authenticated read (lists are public, but writes need the token)
curl -X PATCH localhost:8080/api/users/<id> \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Alice A."}'
```

## What we built

| Capability                  | How                                                 |
| --------------------------- | --------------------------------------------------- |
| Sign-up                     | `POST /api/users` + `AllowPublicWrite` exception    |
| Password hashing            | `service.HashField("password")` on the Service step |
| Bearer-token auth on writes | `auth.JWTAuth` on the Auth step                     |
| Admin-only delete           | `auth.RequireRole("admin")`                         |
| Token issuance              | `/api/auth/login` action                            |

## Next

In **[Part 3 — Modeling Domain Entities & Relations](3-models.md)** we add
the catalogue: `Author`, `Genre`, `Book`, and `Review`, wired up with
`BelongsTo`, `HasMany`, and many-to-many relations.
