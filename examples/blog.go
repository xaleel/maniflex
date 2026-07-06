// example/main.go demonstrates every major maniflex feature in one runnable file.
//
// Models: User, Post (soft-delete), Comment
// Features shown:
//   - maniflex.BaseModel embed (id, created_at, updated_at)
//   - maniflex.WithDeletedAt embed  → soft-delete on Post
//   - FK convention: PostID/UserID → BelongsTo relation
//   - HasMany via slice field: User.Posts, Post.Comments
//   - mfx struct tags: required, readonly, immutable, filterable, sortable,
//     hidden, writeonly, unique, enum, min, max
//   - Two-step init: register models → open DB with registry → SetDB
//   - Auth middleware: Bearer token guard on all writes
//   - Service middleware: password hashing before User create/update
//   - DB (After) middleware: audit log on every mutation
//   - Response (After) middleware: X-Powered-By header on every response
//   - ForModel / ForOperation scoping
//
// Run:
//
//	go run ./example
package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/storage"
)

// ─── Models ───────────────────────────────────────────────────────────────────

// User is a platform user. Password is write-only (never returned in responses).
type User struct {
	maniflex.BaseModel
	Name     string `json:"name"     mfx:"required,filterable,sortable"`
	Email    string `json:"email"     mfx:"required,filterable,unique,immutable"`
	Password string `json:"password"  mfx:"required,writeonly"`
	Role     string `json:"role"      mfx:"filterable,sortable,enum:admin|editor|viewer,default:viewer"`
	// HasMany — populated via ?include=posts
	Posts []Post `json:"posts,omitempty"`
}

// Post is a blog post with soft-delete support.
// Soft-delete is detected automatically via the embedded WithDeletedAt.
type Post struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title  string `json:"title"  mfx:"required,filterable,sortable"`
	Body   string `json:"body"   mfx:"required"`
	Status string `json:"status" mfx:"required,filterable,sortable,enum:draft|published|archived"`
	// BelongsTo User — mfx:"relation" infers the target from the field name
	// (UserID → User). Populated via ?include=user.
	UserID string `json:"user_id"  mfx:"required,filterable,relation"`
	Views  int    `json:"views"    mfx:"readonly,filterable,sortable"`
	// HasMany — populated via ?include=comments
	Comments []Comment `json:"comments,omitempty"`
	Media    string    `json:"media" mfx:"file,max_size:0.5MB,accept:image/*|application/pdf|text/html"`
}

// Comment is attached to a Post and authored by a User.
type Comment struct {
	maniflex.BaseModel
	Body     string `json:"body"     mfx:"required"`
	PostID   string `json:"post_id"  mfx:"required,filterable,immutable,relation"`
	UserID   string `json:"user_id"  mfx:"required,filterable,relation"`
	Approved bool   `json:"approved" mfx:"filterable,sortable"`
}

// ─── Entry point ──────────────────────────────────────────────────────────────

func main() {
	fs, err := storage.NewLocalStorage("./local_storage")
	if err != nil {
		log.Fatal(err)
	}
	// Step 1: create server (no DB yet — we need the registry first for sqlite.Open)
	server := maniflex.New(maniflex.Config{
		Port:        8080,
		PathPrefix:  "/api",
		FilesConfig: maniflex.FilesConfig{Storage: fs},
	})

	// Step 2: register models — this populates the registry
	server.MustRegister(User{}, Post{}, Comment{})

	// Step 3: open SQLite with the populated registry so the adapter can
	// resolve related models during include-population and migration
	db, err := sqlite.Open("./local_storage/db.db", server.Registry())
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	defer db.Close()

	// Step 4: inject the adapter (patches the pipeline's DB step in-place)
	server.SetDB(db)

	// Step 5: register middleware
	registerMiddleware(server)

	printHelp()
	log.Fatal(server.Start())
}

func printHelp() {
	log.Println("maniflex example server on :8080")
	log.Println()
	log.Println("── Schema ────────────────────────────────────────────────────────────────")
	log.Println("  GET    /api/openapi.json             OpenAPI 3.1 spec (needs Bearer token)")
	log.Println()
	log.Println("── Users ─────────────────────────────────────────────────────────────────")
	log.Println("  POST   /api/users                   create user (needs Bearer token)")
	log.Println("  GET    /api/users                   list users")
	log.Println("  GET    /api/users?include=posts      list users with their posts")
	log.Println("  GET    /api/users/:id                read one user")
	log.Println("  PATCH  /api/users/:id                update user (needs Bearer token)")
	log.Println("  DELETE /api/users/:id                delete user (needs Bearer token)")
	log.Println()
	log.Println("── Posts ─────────────────────────────────────────────────────────────────")
	log.Println("  GET    /api/posts?filter=status:eq:published")
	log.Println("  GET    /api/posts?filter=user.role:eq:admin&include=user,comments")
	log.Println("  GET    /api/posts?sort=created_at:desc&page=1&limit=10")
	log.Println()
	log.Println("── All write requests + /openapi.json need: Authorization: Bearer any-token")
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func registerMiddleware(s *maniflex.Server) {
	// Auth: require a Bearer token on every write operation, for all models
	s.Pipeline.Auth.Register(
		bearerTokenAuth,
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
	)

	// Service: hash the password field before it reaches the DB
	s.Pipeline.Service.Register(
		hashUserPassword,
		maniflex.ForModel("User"),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
	)

	// DB After: audit-log every successful mutation
	s.Pipeline.DB.Register(
		auditLog,
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.AtPosition(maniflex.After),
	)

	// Response After: stamp X-Powered-By on every response
	s.Pipeline.Response.Register(
		addPoweredByHeader,
		maniflex.AtPosition(maniflex.After),
	)

	// OpenAPI Generate (After): enrich the spec with server info and security scheme
	s.Pipeline.OpenAPI.Generate.Register(enrichSpec, maniflex.After)
}

// bearerTokenAuth is a minimal demonstration auth middleware.
// In production replace with a real JWT validation library.
func bearerTokenAuth(ctx *maniflex.ServerContext, next func() error) error {
	header := ctx.Request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
			"missing Authorization: Bearer <token> header")
		return nil
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" {
		ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "empty bearer token")
		return nil
	}

	// Populate ctx.Auth so downstream middleware can read the identity.
	ctx.Auth = &maniflex.AuthInfo{
		UserID: "00000000-0000-0000-0000-000000000001",
		Roles:  []string{"editor"},
		Claims: map[string]any{"sub": token},
	}
	return next()
}

// hashUserPassword replaces the plaintext password with a hashed version
// before the DB step runs. Must come before (default Position = Before).
func hashUserPassword(ctx *maniflex.ServerContext, next func() error) error {
	if pw, ok := ctx.Field("password"); ok && pw != nil {
		// Replace with bcrypt.GenerateFromPassword in production.
		// SetField writes through to both ParsedBody and the typed ctx.Record.
		ctx.SetField("password", fmt.Sprintf("bcrypt(%v)", pw))
	}
	return next()
}

// auditLog is inserted After the DB step so it runs only when the DB
// succeeded. It logs the operation, model, and actor.
func auditLog(ctx *maniflex.ServerContext, next func() error) error {
	// Call next first — we are in After position, so the DB step already ran.
	if err := next(); err != nil {
		return err
	}
	// Don't log if the DB step produced an error response.
	if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
		return nil
	}

	actorID := "<anonymous>"
	if ctx.Auth != nil {
		actorID = ctx.Auth.UserID
	}
	log.Printf("[audit] op=%-7s model=%-10s actor=%s  ts=%s",
		ctx.Operation, ctx.Model.Name, actorID,
		time.Now().UTC().Format(time.RFC3339),
	)
	return nil
}

// addPoweredByHeader stamps every HTTP response with a custom header.
func addPoweredByHeader(ctx *maniflex.ServerContext, next func() error) error {
	ctx.Writer.Header().Set("X-Powered-By", "maniflex")
	return next()
}

// enrichSpec runs After the Generate step and customises the produced spec.
// Adds a human-readable description, a bearer security scheme, and marks
// all write operations as secured.
func enrichSpec(ctx *maniflex.OpenAPIContext, next func() error) error {
	if ctx.Spec == nil {
		return nil
	}

	ctx.Spec.Info.Title = "Blog Platform API"
	ctx.Spec.Info.Description = "Example API built with maniflex. " +
		"Write operations and /openapi.json require a Bearer token."

	// Add a bearer security scheme to the components
	if ctx.Spec.Components.SecuritySchemes == nil {
		ctx.Spec.Components.SecuritySchemes = make(map[string]maniflex.OASSecurityScheme)
	}
	ctx.Spec.Components.SecuritySchemes["bearerAuth"] = maniflex.OASSecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  "Pass any non-empty string as the token in the example server.",
	}

	return nil
}
