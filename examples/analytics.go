//go:build ignore

// examples/analytics.go — Analytics backend built with maniflex.
//
// Domain: a minimal web analytics platform where site owners manage tracked
// websites and view aggregate reports, while browser clients ingest events
// anonymously — the same model used by Plausible and Fathom Analytics.
//
// maniflex features demonstrated:
//
//	2.4  maniflex.ConfigFromEnv("ANALYTICS")
//	     Reads ANALYTICS_PORT, ANALYTICS_SERVICE_NAME, etc. from the
//	     environment so production config requires no code changes.
//
//	2.6  RS256 JWT authentication (asymmetric key)
//	     Read endpoints require a JWT signed with an RSA private key.
//	     Event ingestion (POST) is intentionally open — any page can POST
//	     events without credentials.
//
//	3D.1 server.Action()
//	     Three custom endpoints outside the standard CRUD surface:
//	       GET  /sites/{id}/stats      — aggregate metrics for a site
//	       GET  /sites/{id}/top-pages  — top pages by pageview count
//	       POST /ingest                — anonymous event ingestion by token
//
//	3D.2 ctx.RawQuery / RawExec
//	     GROUP BY aggregations inside action handlers — cases where the
//	     query builder cannot yet express what is needed.
//
//	3D.3 ctx.QueryModel
//	     Cross-model reads inside action handlers, participating in the
//	     active transaction when one is present.
//
//	Also shown:
//	  - HasMany / BelongsTo relations with ?include=
//	  - Soft-delete on Site (WithDeletedAt)
//	  - service.SetField for server-side computed fields
//	  - db.RateLimit for anonymous write throttling
//	  - Ownership scoping via ForceFilter pattern
//	  - maniflex.WithTransaction for request-level transactions
//	  - Structured logging via ctx.Logger()
//	  - ctx.HasRole for role checks inside action handlers
//	  - Response (After) CORS middleware for cross-origin tracker scripts
//	  - OpenAPI spec enrichment
//
// Run:
//
//	go run examples/analytics.go
//
// A fresh RS256 key pair is generated on every start. The startup log
// prints ready-to-use Bearer tokens for admin and viewer identities so
// you can exercise every endpoint without any external identity provider.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/middleware/auth"
	mwdb "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/service"
)

// ─── Models ───────────────────────────────────────────────────────────────────

// Site represents a tracked website. Each site has a secret ingestion token
// that browser tracker scripts use to submit events without credentials.
type Site struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt // soft-delete: DELETE sets deleted_at, not a hard remove

	Name   string `json:"name"   mfx:"required,filterable,sortable"`
	Domain string `json:"domain" mfx:"required,unique,filterable,sortable"`

	// Token is the ingestion secret embedded in tracker scripts.
	// readonly: clients cannot supply or overwrite it; the server generates it.
	Token string `json:"token" mfx:"readonly"`

	// OwnerID is injected server-side from ctx.Auth.UserID on create.
	// Not marked required because the value arrives via Service middleware,
	// which runs after Validate — supplying it in the request body is optional.
	OwnerID string `json:"owner_id" mfx:"filterable,sortable"`

	// HasMany — populated when ?include=events is appended to the request URL.
	Events []Event `json:"events,omitempty"`
}

// Event is a single tracked user interaction: a page view, click, etc.
// Events are immutable once created (EventType and SiteID carry immutable tags).
type Event struct {
	maniflex.BaseModel

	// BelongsTo Site — FK by naming convention: SiteID field → Site companion.
	// No companion struct field needed; ?include=site still works. The framework
	// detects the relation from SiteID and inlines the fetched record into the
	// JSON response under the "site" key.
	SiteID string `json:"site_id" mfx:"required,filterable,sortable,immutable"`

	EventType   string `json:"event_type"   mfx:"required,filterable,sortable,immutable,enum:pageview|click|conversion|custom"`
	PageURL     string `json:"page_url"     mfx:"required,filterable"`
	RemoteAddr  string `json:"remote_addr"  mfx:"required,default:"`
	SessionID   string `json:"session_id"   mfx:"required,filterable"`
	Country     string `json:"country"      mfx:"filterable,sortable"`
	ReferrerURL string `json:"referrer_url" mfx:"filterable"`

	// Properties stores arbitrary JSON metadata from the tracker script.
	// writeonly: accepted on create but excluded from responses to keep payloads lean.
	Properties string `json:"properties" mfx:"writeonly"`
}

// ─── Entry point ──────────────────────────────────────────────────────────────

func main() {
	// ── 2.4: Load config from environment ────────────────────────────────────
	//
	// ConfigFromEnv reads any ANALYTICS_* variable that maps to a Config field.
	// Variables not set in the environment leave the corresponding field at its
	// zero value; the code below applies defaults for those cases. A variable that
	// IS set but cannot be read (ANALYTICS_PORT=808O) is an error — better to stop
	// here than to boot on a port nobody meant.
	//
	// Production deployment example (Docker / Kubernetes):
	//   ANALYTICS_PORT=8080
	//   ANALYTICS_SERVICE_NAME=analytics
	//   ANALYTICS_DB_WRITE_URL=postgres://...
	//   ANALYTICS_QUERY_TIMEOUT_MS=5000
	//   ANALYTICS_HEALTH_CHECK_DB=true
	cfg, err := maniflex.ConfigFromEnv("ANALYTICS")
	if err != nil {
		log.Fatal(err)
	}

	cfg.HealthCheckDB = true
	if cfg.ServiceName == "" {
		cfg.ServiceName = "analytics"
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// ── 2.6: Generate RS256 key pair ─────────────────────────────────────────
	//
	// In production: load the public key from your identity provider's JWKS
	// endpoint or a PEM file:
	//
	//   block, _ := pem.Decode([]byte(os.Getenv("JWT_PUBLIC_KEY_PEM")))
	//   pub, _   := x509.ParsePKIXPublicKey(block.Bytes)
	//   rsaPub   := pub.(*rsa.PublicKey)
	//
	// Here we generate a fresh 2048-bit key pair so the example is self-contained.
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate RSA key: %v", err)
	}
	rsaPub := &privateKey.PublicKey

	// Step 1: create server (no DB yet — registry must be populated first)
	server := maniflex.New(cfg)

	// Step 2: register models — populates the registry sqlite.Open needs
	server.MustRegister(Site{}, Event{})

	// Step 3: open SQLite with the populated registry
	sqliteDB, err := sqlite.Open("./analytics.db", server.Registry())
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	defer sqliteDB.Close()
	server.SetDB(sqliteDB)

	// Step 4: capture Event model meta before registering actions.
	// Action handlers are closures — they capture this pointer at registration
	// time and use it to call tx.Create inside the ingest handler.
	eventMeta, ok := server.Registry().Get("Event")
	if !ok {
		log.Fatal("Event model not in registry")
	}

	// Step 5: register pipeline middleware
	registerMiddleware(server, rsaPub)

	// Step 6: register custom action endpoints (3D.1)
	// Must be called before server.Start() / server.Handler().
	registerActions(server, rsaPub, eventMeta)

	printHelp(privateKey)
	log.Fatal(server.Start())
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func registerMiddleware(s *maniflex.Server, rsaPub *rsa.PublicKey) {
	// ── Auth: RS256 JWT for read operations ───────────────────────────────────
	//
	// 2.6: JWTAuth verifies the token signature using the RSA public key.
	//      Setting PublicKey enables the RS256/ES256 path; the secret string
	//      is ignored.
	//
	// Scoped to OpRead + OpList so event ingestion remains open.
	// Any identity provider (Auth0, Keycloak, Cognito) that issues RS256 tokens
	// will work here — replace rsaPub with the provider's public key.
	s.Pipeline.Auth.Register(
		auth.JWTAuth("", auth.JWTOptions{
			PublicKey:   rsaPub, // RS256 asymmetric verification
			UserIDClaim: "sub",
			RolesClaim:  "roles",
		}),
		maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
		maniflex.WithName("RS256ReadAuth"),
	)

	// Site management writes (create/update/delete) also require a valid JWT.
	// Registering this separately from the read scope lets you later swap site
	// creation to a different scheme (e.g. API-key auth) without touching reads.
	s.Pipeline.Auth.Register(
		auth.JWTAuth("", auth.JWTOptions{
			PublicKey:   rsaPub,
			UserIDClaim: "sub",
			RolesClaim:  "roles",
		}),
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.WithName("SiteManagementAuth"),
	)

	// Only admin users may delete sites. Soft-delete (WithDeletedAt) means
	// DELETE sets deleted_at rather than removing the row, but the operation
	// still requires this role check.
	s.Pipeline.Auth.Register(
		auth.RequireRole("admin"),
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpDelete),
		maniflex.WithName("AdminDeleteGuard"),
	)

	// ── Service: server-side computed fields ─────────────────────────────────

	// Generate a 32-hex-char ingestion token before each site is created.
	// Token is "readonly" in the model so clients cannot supply or overwrite it.
	s.Pipeline.Service.Register(
		generateSiteToken,
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpCreate),
		maniflex.WithName("SiteTokenGenerator"),
	)

	// Inject owner_id from the authenticated user so the field cannot be spoofed.
	// service.SetField runs in the Service step (after Validate) so the required
	// check on name/domain fires first, then owner_id is injected before the DB write.
	s.Pipeline.Service.Register(
		service.SetField("owner_id", func(ctx *maniflex.ServerContext) any {
			if ctx.Auth != nil {
				return ctx.Auth.UserID
			}
			return ""
		}),
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpCreate),
		maniflex.WithName("OwnerInjector"),
	)

	// Wrap all Site mutations in a request-level transaction.
	// maniflex.WithTransaction opens a tx, sets ctx.Tx, then defers Rollback.
	// The DB step and all After-DB middleware see the same transaction.
	s.Pipeline.Service.Register(
		maniflex.WithTransaction(nil),
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.WithName("SiteTransaction"),
	)

	// ── DB: ownership scoping on reads ────────────────────────────────────────
	//
	// Admin users see all sites. Regular authenticated users see only their own.
	// This Before-DB middleware appends a WHERE owner_id = ? filter; the
	// framework merges it with any client-supplied filters before querying.
	s.Pipeline.DB.Register(
		siteOwnershipFilter,
		maniflex.ForModel("Site"),
		maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
		maniflex.WithName("SiteOwnershipFilter"),
	)

	// ── DB: rate-limit anonymous event ingestion ─────────────────────────────
	//
	// CRUD POST /events is open, so we key the rate limiter on the remote IP.
	// For production across replicas, set RateLimitConfig.Backend to a shared
	// counter (see middleware/db/redis).
	s.Pipeline.DB.Register(
		mwdb.RateLimit(mwdb.RateLimitConfig{
			RequestsPerMinute: 10,
			KeyFunc: func(ctx *maniflex.ServerContext) string {
				return ctx.Request.RemoteAddr
			},
			ErrorMessage: "event ingestion rate limit exceeded — back off and retry",
		}),
		maniflex.ForModel("Event"),
		maniflex.ForOperation(maniflex.OpCreate),
		maniflex.WithName("IngestRateLimit"),
	)

	// ── DB After: structured audit log ───────────────────────────────────────
	//
	// Runs after every successful mutation. ctx.Auth may be nil for anonymous
	// event writes; we fall back to logging the remote IP in that case.
	// AtPosition(After) means it only fires when the DB step succeeded.
	s.Pipeline.DB.Register(
		auditMutation,
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.AtPosition(maniflex.After),
		maniflex.WithName("AuditLog"),
	)

	// ── Response After: CORS ─────────────────────────────────────────────────
	//
	// Allows browser tracker scripts to POST to /api/ingest cross-origin.
	// In production, replace "*" with your allowed origin list.
	s.Pipeline.Response.Register(
		addCORSHeaders,
		maniflex.AtPosition(maniflex.After),
		maniflex.WithName("CORSHeaders"),
	)

	// ── OpenAPI: enrich the auto-generated spec ───────────────────────────────
	s.Pipeline.OpenAPI.Generate.Register(enrichSpec, maniflex.After)
}

// ─── Custom action endpoints (3D.1) ──────────────────────────────────────────

// registerActions adds three endpoints that go beyond the standard CRUD surface.
// Two require the same RS256 JWT used by read operations; one is anonymous.
func registerActions(s *maniflex.Server, rsaPub *rsa.PublicKey, eventMeta *maniflex.ModelMeta) {
	// Reusable JWT middleware for the two authenticated GET actions.
	// Per-action middleware runs between the global Auth step and the handler,
	// so global middleware registered on the Auth pipeline still fires first.
	jwtAuth := auth.JWTAuth("", auth.JWTOptions{
		PublicKey:   rsaPub,
		UserIDClaim: "sub",
		RolesClaim:  "roles",
	})

	// GET /sites/{id}/stats
	// Demonstrates 3D.3 ctx.QueryModel (ownership check) and
	// 3D.2 ctx.RawQuery (aggregate counts).
	s.Action(maniflex.ActionConfig{
		Method:     "GET",
		Path:       "/sites/{id}/stats",
		Tags:       []string{"Analytics"},
		Summary:    "Aggregate metrics for a site (events, sessions, conversions)",
		Middleware: []maniflex.MiddlewareFunc{jwtAuth},
		Handler:    siteStatsHandler,
		Responses: map[int]*maniflex.OASSchema{
			200: {
				Type: "object",
				Properties: map[string]*maniflex.OASSchema{
					"site": {Ref: "#/components/schemas/Site"},
					"stats": {
						Type: "object",
						Properties: map[string]*maniflex.OASSchema{
							"total_events":    {Type: "integer"},
							"unique_sessions": {Type: "integer"},
							"pageviews":       {Type: "integer"},
							"conversions":     {Type: "integer"},
							"clicks":          {Type: "integer"},
						},
					},
				},
			},
			401: nil,
			403: nil,
			404: nil,
		},
	})

	// GET /sites/{id}/top-pages
	// Demonstrates 3D.2 ctx.RawQuery with GROUP BY and optional query params.
	s.Action(maniflex.ActionConfig{
		Method:     "GET",
		Path:       "/sites/{id}/top-pages",
		Tags:       []string{"Analytics"},
		Summary:    "Top pages ranked by pageview count (optional ?limit=N, max 100)",
		Middleware: []maniflex.MiddlewareFunc{jwtAuth},
		Handler:    topPagesHandler,
		Responses: map[int]*maniflex.OASSchema{
			200: {
				Type: "array",
				Items: &maniflex.OASSchema{
					Type: "object",
					Properties: map[string]*maniflex.OASSchema{
						"page_url":        {Type: "string"},
						"views":           {Type: "integer"},
						"unique_visitors": {Type: "integer"},
					},
				},
			},
			401: nil,
			403: nil,
		},
	})

	// POST /ingest — intentionally no Middleware slice.
	// Browser tracker scripts call this anonymously with just the site token.
	// Demonstrates 3D.1 (anonymous action) + 3D.3 ctx.QueryModel (site lookup)
	// + ctx.BeginTx for a transactional write.
	s.Action(maniflex.ActionConfig{
		Method:  "POST",
		Path:    "/ingest",
		Tags:    []string{"Ingestion"},
		Summary: "Anonymous event ingestion — authenticate with the site's token, not a JWT",
		Handler: ingestHandler(eventMeta),
		RequestBody: maniflex.JSONRequestBody(&maniflex.OASSchema{
			Type: "object",
			Properties: map[string]*maniflex.OASSchema{
				"site_token":   {Type: "string"},
				"event_type":   {Type: "string", Enum: []any{"pageview", "click", "conversion", "custom"}},
				"page_url":     {Type: "string"},
				"session_id":   {Type: "string"},
				"country":      {Type: "string"},
				"referrer_url": {Type: "string"},
				"properties":   {Type: "string", Description: "Arbitrary JSON metadata from the tracker script"},
			},
			Required: []string{"site_token", "event_type", "page_url", "session_id"},
		}),
		Responses: map[int]*maniflex.OASSchema{
			201: {
				Type: "object",
				Properties: map[string]*maniflex.OASSchema{
					"id":      {Type: "string", Format: "uuid"},
					"site_id": {Type: "string", Format: "uuid"},
				},
			},
			400: nil,
			401: nil,
		},
	})
}

// siteStatsHandler returns aggregate metrics for a single site.
//
// 3D.3 ctx.QueryModel: verify the site exists and is accessible to the caller.
//
// 3D.2 ctx.RawQuery: run a CASE-based aggregate that the query builder cannot
// yet express (roadmap 4.5 ctx.Aggregate is still pending).
func siteStatsHandler(ctx *maniflex.ServerContext) error {
	siteID := ctx.ResourceID

	// Build the site lookup filter.
	// Admin users can view any site's stats; regular users only their own.
	// ctx.HasRole is a convenience helper on ServerContext.
	filters := []*maniflex.FilterExpr{
		{Field: "id", Operator: maniflex.OpEq, Value: siteID},
	}
	if !ctx.HasRole("admin") {
		filters = append(filters, &maniflex.FilterExpr{
			Field: "owner_id", Operator: maniflex.OpEq, Value: ctx.Auth.UserID,
		})
	}

	// 3D.3: QueryModel reads from the Site model using the standard FindMany
	// path (including any active transaction). This is the right tool when you
	// need cross-model reads inside a custom handler — it respects soft-delete
	// and the model's field mapping automatically.
	sites, err := ctx.QueryModel("Site", &maniflex.QueryParams{
		Filters: filters,
		Page:    1,
		Limit:   1,
	})

	if err != nil {
		return fmt.Errorf("site lookup: %w", err)
	}
	if len(sites) == 0 {
		ctx.Abort(http.StatusNotFound, "NOT_FOUND",
			"site not found or not accessible to this user")
		return nil
	}

	// 3D.2: RawQuery for aggregate counts.
	// CASE expressions and GROUP BY aggregations are not yet supported by the
	// query builder (roadmap 4.5). ctx.RawQuery accepts any parameterised SQL
	// and routes through ctx.Tx when a transaction is active.
	rows, err := ctx.RawQuery(`
		SELECT
			COALESCE(COUNT(*), 0)                                              AS total_events,
			COALESCE(COUNT(DISTINCT session_id), 0)                            AS unique_sessions,
			COALESCE(COUNT(CASE WHEN event_type = 'pageview'   THEN 1 END), 0) AS pageviews,
			COALESCE(COUNT(CASE WHEN event_type = 'conversion' THEN 1 END), 0) AS conversions,
			COALESCE(COUNT(CASE WHEN event_type = 'click'      THEN 1 END), 0) AS clicks
		FROM events
		WHERE site_id = ?
	`, siteID)
	if err != nil {
		return fmt.Errorf("stats query: %w", err)
	}

	var stats map[string]any
	if len(rows) > 0 {
		stats = rows[0]
	}

	// ctx.Logger() returns a *slog.Logger pre-seeded with request_id,
	// service name, and trace_id so every log line is automatically correlated
	// to the originating HTTP request without extra boilerplate.
	ctx.Logger().Info("stats fetched",
		slog.String("site_id", siteID),
		slog.String("user_id", ctx.Auth.UserID),
	)

	ctx.Response = &maniflex.APIResponse{
		StatusCode: http.StatusOK,
		Data: map[string]any{
			"site":  sites[0],
			"stats": stats,
		},
	}
	return nil
}

// topPagesHandler returns the top N pages ranked by pageview count.
//
// 3D.2 ctx.RawQuery: GROUP BY query with an optional ?limit= query param.
func topPagesHandler(ctx *maniflex.ServerContext) error {
	siteID := ctx.ResourceID

	// ctx.QueryParam is a convenience wrapper around r.URL.Query().Get().
	limit := 10
	if l := ctx.QueryParam("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	rows, err := ctx.RawQuery(`
		SELECT
			page_url,
			COUNT(*)               AS views,
			COUNT(DISTINCT session_id) AS unique_visitors
		FROM events
		WHERE site_id = ? AND event_type = 'pageview'
		GROUP BY page_url
		ORDER BY views DESC
		LIMIT ?
	`, siteID, limit)
	if err != nil {
		return fmt.Errorf("top-pages query: %w", err)
	}

	if rows == nil {
		ctx.Logger().Info("ROWS NIL")
		rows = []map[string]any{} // return [] not null for empty results
	}

	ctx.Response = &maniflex.APIResponse{
		StatusCode: http.StatusOK,
		Data:       rows,
	}
	return nil
}

// ingestHandler returns an action handler that accepts events via site token.
// The site token is embedded in the tracker snippet — no user JWT needed.
//
// 3D.1 Action:      custom endpoint, anonymous, outside the CRUD surface.
// 3D.2 RawQuery:    look up the site by its token without a filterable field.
// ctx.BeginTx:      transactional write so the response ID is always committed.
func ingestHandler(eventMeta *maniflex.ModelMeta) maniflex.ActionHandlerFunc {
	return func(ctx *maniflex.ServerContext) error {
		var req struct {
			SiteToken   string `json:"site_token"`
			EventType   string `json:"event_type"`
			PageURL     string `json:"page_url"`
			SessionID   string `json:"session_id"`
			Country     string `json:"country"`
			ReferrerURL string `json:"referrer_url"`
			Properties  string `json:"properties"`
			RemoteAddr  string
		}
		// ctx.BindJSON reads the body, enforces the 4 MB limit, and calls
		// ctx.Abort on any error — the handler just returns nil after a failure.
		if err := ctx.BindJSON(&req); err != nil {
			return nil
		}
		if req.SiteToken == "" || req.EventType == "" || req.PageURL == "" || req.SessionID == "" {
			ctx.Abort(http.StatusBadRequest, "MISSING_FIELDS",
				"site_token, event_type, page_url, and session_id are required")
			return nil
		}

		// Look up the site by token using raw SQL.
		// Token is not marked filterable in the model (which would expose it to
		// HTTP ?filter= params). ctx.RawQuery bypasses the filterable check and
		// lets us query on any column safely with parameterised placeholders.
		//
		// The deleted_at IS NULL condition honours soft-delete — deactivated
		// sites cannot receive new events.
		sitesRaw, err := ctx.RawQuery(
			"SELECT id FROM sites WHERE token = ? AND deleted_at IS NULL",
			req.SiteToken,
		)
		if err != nil {
			return fmt.Errorf("site token lookup: %w", err)
		}
		if len(sitesRaw) == 0 {
			ctx.Abort(http.StatusUnauthorized, "INVALID_TOKEN",
				"no active site matches the provided site_token")
			return nil
		}
		siteID, _ := sitesRaw[0]["id"].(string)

		// Wrap the insert in a transaction so the caller receives the generated
		// ID only after a confirmed commit. defer tx.Rollback() is a no-op once
		// Commit succeeds.
		tx, err := ctx.BeginTx(ctx.Ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback()

		event, err := tx.Create(ctx.Ctx, eventMeta, map[string]any{
			"site_id":      siteID,
			"event_type":   req.EventType,
			"page_url":     req.PageURL,
			"session_id":   req.SessionID,
			"country":      req.Country,
			"referrer_url": req.ReferrerURL,
			"properties":   req.Properties,
			"remote_addr":  ctx.Request.RemoteAddr,
		})
		if err != nil {
			return fmt.Errorf("create event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		ctx.Logger().Info("event ingested",
			slog.String("site_id", siteID),
			slog.String("event_type", req.EventType),
		)

		ctx.Response = &maniflex.APIResponse{
			StatusCode: http.StatusCreated,
			Data: map[string]any{
				"id":      maniflex.RecordToMap(eventMeta, event)["id"],
				"site_id": siteID,
			},
		}
		return nil
	}
}

// ─── Helper middleware ────────────────────────────────────────────────────────

// generateSiteToken injects a 32-hex-char random ingestion token before the
// DB write. Token is "readonly" in the model so clients cannot supply it.
func generateSiteToken(ctx *maniflex.ServerContext, next func() error) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("token gen: %w", err)
	}
	ctx.SetField("token", fmt.Sprintf("%x", b))
	return next()
}

// siteOwnershipFilter restricts site list/read to records owned by the caller.
// Admin users bypass the filter and see all sites.
//
// This is the ForceFilter pattern: append a FilterExpr to ctx.Query.Filters
// before the DB step runs. The DB step merges it with any client-supplied
// filters so the caller cannot escape the restriction.
func siteOwnershipFilter(ctx *maniflex.ServerContext, next func() error) error {
	if ctx.Auth == nil {
		return next()
	}
	if ctx.HasRole("admin") {
		return next() // admins see all sites
	}
	if ctx.Query == nil {
		ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
	}
	ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
		Field:    "owner_id",
		Operator: maniflex.OpEq,
		Value:    ctx.Auth.UserID,
	})
	return next()
}

// auditMutation logs the actor, model, and operation for every successful write.
// Registered at AtPosition(After) so it only fires when the DB step succeeded.
func auditMutation(ctx *maniflex.ServerContext, next func() error) error {
	if err := next(); err != nil {
		return err
	}
	if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
		return nil
	}
	actor := "<anonymous>"
	if ctx.Auth != nil && ctx.Auth.UserID != "" {
		actor = ctx.Auth.UserID
	}
	ctx.Logger().Info("mutation",
		slog.String("op", string(ctx.Operation)),
		slog.String("model", ctx.Model.Name),
		slog.String("actor", actor),
	)
	return nil
}

// addCORSHeaders lets browser tracker scripts POST to /api/ingest cross-origin.
func addCORSHeaders(ctx *maniflex.ServerContext, next func() error) error {
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
	ctx.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	ctx.Writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	return next()
}

// enrichSpec annotates the auto-generated OpenAPI spec with titles and the
// bearer security scheme so API clients know what auth to use.
func enrichSpec(ctx *maniflex.OpenAPIContext, next func() error) error {
	if ctx.Spec == nil {
		return nil
	}
	ctx.Spec.Info.Title = "Analytics API"
	ctx.Spec.Info.Description =
		"Web analytics backend built with maniflex. " +
			"Read endpoints require an RS256-signed JWT. " +
			"POST /ingest is intentionally open for anonymous browser trackers."
	if ctx.Spec.Components.SecuritySchemes == nil {
		ctx.Spec.Components.SecuritySchemes = make(map[string]maniflex.OASSecurityScheme)
	}
	ctx.Spec.Components.SecuritySchemes["bearerAuth"] = maniflex.OASSecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  "RS256-signed JWT. Copy a test token from the server startup log.",
	}
	return nil
}

// ─── RSA / JWT helpers ────────────────────────────────────────────────────────

// makeTestJWT produces a signed RS256 JWT for testing.
// In production tokens come from your identity provider (Auth0, Keycloak, etc.).
func makeTestJWT(key *rsa.PrivateKey, sub string, roles []string, ttl time.Duration) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"sub":   sub,
		"roles": roles,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(ttl).Unix(),
	})

	h := base64.RawURLEncoding.EncodeToString(hdr)
	p := base64.RawURLEncoding.EncodeToString(payload)
	msg := h + "." + p

	digest := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		log.Fatalf("sign JWT: %v", err)
	}
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// ─── Help ─────────────────────────────────────────────────────────────────────

func printHelp(privateKey *rsa.PrivateKey) {
	adminToken := makeTestJWT(privateKey, "usr-admin-001", []string{"admin"}, 24*time.Hour)
	viewerToken := makeTestJWT(privateKey, "usr-viewer-002", []string{"viewer"}, 24*time.Hour)

	log.Println("── Analytics example on :8080 ─────────────────────────────────────────────")
	log.Println()
	log.Printf("Admin  token: Bearer %s", adminToken)
	log.Println()
	log.Printf("Viewer token: Bearer %s", viewerToken)
	log.Println()
	log.Println("── Sites (require RS256 JWT) ──────────────────────────────────────────────")
	log.Println("  POST   /api/sites                     create site (JWT required)")
	log.Println("  GET    /api/sites                     list your sites")
	log.Println("  GET    /api/sites?include=events       list sites with their events")
	log.Println("  GET    /api/sites/:id                 read site")
	log.Println("  PATCH  /api/sites/:id                 update site")
	log.Println("  DELETE /api/sites/:id                 soft-delete (admin only)")
	log.Println("  GET    /api/sites/:id/stats           aggregate metrics  [3D.1 Action]")
	log.Println("  GET    /api/sites/:id/top-pages       top pages          [3D.1 Action]")
	log.Println()
	log.Println("── Events (reads require JWT; writes are anonymous) ───────────────────────")
	log.Println("  POST   /api/events                    create event (anonymous CRUD)")
	log.Println("  GET    /api/events                    list events")
	log.Println("  GET    /api/events?filter=event_type:eq:pageview&sort=created_at:desc")
	log.Println("  POST   /api/ingest                    anonymous ingest by site token [3D.1]")
	log.Println()
	log.Println("── Schema & health ────────────────────────────────────────────────────────")
	log.Println("  GET    /api/openapi.json              OpenAPI 3.1 spec")
	log.Println("  GET    /api/health                    health check (DB ping)")
	log.Println()
	log.Println("── Quick start ────────────────────────────────────────────────────────────")
	log.Println("  # 1. Create a site (admin token):")
	log.Println(`       curl -s -X POST http://localhost:8080/api/sites \`)
	log.Println(`            -H "Authorization: Bearer <adminToken>" \`)
	log.Println(`            -H "Content-Type: application/json" \`)
	log.Println(`            -d '{"name":"My Blog","domain":"blog.example.com"}' | jq .`)
	log.Println()
	log.Println("  # Copy the token from the create response, then ingest an event:")
	log.Println("  # 2. Ingest a pageview (no JWT needed):")
	log.Println(`       curl -s -X POST http://localhost:8080/api/ingest \`)
	log.Println(`            -H "Content-Type: application/json" \`)
	log.Println(`            -d '{"site_token":"<token>","event_type":"pageview","page_url":"/","session_id":"abc123"}' | jq .`)
	log.Println()
	log.Println("  # 3. View aggregate stats for the site (admin token):")
	log.Println(`       curl -s http://localhost:8080/api/sites/<id>/stats \`)
	log.Println(`            -H "Authorization: Bearer <adminToken>" | jq .`)
	log.Println()
	log.Println("  # 4. Top pages report:")
	log.Println(`       curl -s "http://localhost:8080/api/sites/<id>/top-pages?limit=5" \`)
	log.Println(`            -H "Authorization: Bearer <adminToken>" | jq .`)
}
