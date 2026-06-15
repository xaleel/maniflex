```go
import (
    "maniflex/middleware/auth"
    "maniflex/middleware/body"
    "maniflex/middleware/validate"
    "maniflex/middleware/service"
    "maniflex/middleware/db"
    "maniflex/middleware/response"
    "maniflex/middleware/openapi"
)

// ── Auth ──────────────────────────────────────────────────────────────────────
s.Pipeline.Auth.Register(auth.JWTAuth("secret", auth.JWTOptions{Issuer: "myapp"}))
s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key", auth.APIKeyEntry{Key: "abc", Auth: maniflex.AuthInfo{Roles: []string{"admin"}}}))
s.Pipeline.Auth.Register(auth.RequireRole("admin"), maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpDelete))
s.Pipeline.Auth.Register(auth.AllowPublicRead())
s.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete), maniflex.ForModel("AuditLog"))

// ── Body ──────────────────────────────────────────────────────────────────────
s.Pipeline.Deserialize.Register(body.MaxBodySize(16<<20), maniflex.ForModel("Article"))
s.Pipeline.Validate.Register(body.StripUnknownFields())
s.Pipeline.Validate.Register(body.CoerceTypes())

// ── Validate ──────────────────────────────────────────────────────────────────
s.Pipeline.Validate.Register(validate.UniqueField(sqlDB, "email"), maniflex.ForModel("User"))
s.Pipeline.Validate.Register(validate.CrossFieldValidate(func(b map[string]any) error { ... }))
s.Pipeline.Validate.Register(validate.RegexField("phone", `^\+?[0-9]{7,15}$`))
s.Pipeline.Validate.Register(validate.ForbiddenValues("role", "superadmin"))
s.Pipeline.Validate.Register(validate.RequireAtLeastOne("name", "email"), maniflex.ForOperation(maniflex.OpUpdate))
s.Pipeline.Validate.Register(validate.NumericPrecision("amount", 19, 4), maniflex.ForModel("Invoice"))
s.Pipeline.Validate.Register(validate.DateRange("start_date", "end_date"), maniflex.ForModel("Booking"))
s.Pipeline.Validate.Register(validate.RequireWhen("rejection_reason", "status:eq:rejected"), maniflex.ForModel("Claim"))

// ── Service ───────────────────────────────────────────────────────────────────
s.Pipeline.Service.Register(service.HashField("password"), maniflex.ForModel("User"))
s.Pipeline.Service.Register(service.SlugifyField("title", "slug"), maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate))
s.Pipeline.Service.Register(service.SetField("user_id", func(ctx *maniflex.ServerContext) any { return ctx.Auth.UserID }))
s.Pipeline.Service.Register(service.StripField("password_confirm"))
s.Pipeline.Service.Register(service.TimestampWhen("published_at", "status", "published"))
s.Pipeline.Service.Register(service.OwnerScope("user_id"), maniflex.ForOperation(maniflex.OpCreate))
s.Pipeline.DB.Register(service.Emit(myBus), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete), maniflex.AtPosition(maniflex.After))
s.Pipeline.DB.Register(service.Webhook(service.WebhookConfig{URL: "https://hooks.example.com", Secret: "whsec_x"}), maniflex.AtPosition(maniflex.After))
s.Pipeline.DB.Register(service.SendEmail(mailer, func(ctx *maniflex.ServerContext) \*service.EmailMessage { ... }), maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))

// ── DB ────────────────────────────────────────────────────────────────────────
s.Pipeline.DB.Register(db.ForceFilter("org_id", func(ctx *maniflex.ServerContext) any { return ctx.Auth.Claims["org_id"] }))
s.Pipeline.DB.Register(db.Tenancy("org_id", func(ctx *maniflex.ServerContext) string { return ctx.Auth.Claims["org_id"].(string) }))
s.Pipeline.DB.Register(db.Paginate(50), maniflex.ForModel("AuditLog"))
s.Pipeline.DB.Register(db.RateLimit(db.RateLimitConfig{RequestsPerMinute: 10}), maniflex.ForModel("PasswordReset"))
s.Pipeline.DB.Register(db.AuditLog(mySink), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete), maniflex.AtPosition(maniflex.After))
s.Pipeline.DB.Register(db.Invalidate(redisCache, func(ctx \*maniflex.ServerContext) []string { return []string{"posts:list"} }), maniflex.AtPosition(maniflex.After))
s.Pipeline.DB.Register(db.CacheQuery(cache, db.CacheConfig{TTL: 5 * time.Minute, KeyFunc: func(ctx *maniflex.ServerContext) string { return "posts:list:" + ctx.Request.URL.RawQuery }}), maniflex.ForOperation(maniflex.OpList, maniflex.OpRead))

// ── Response ──────────────────────────────────────────────────────────────────
s.Pipeline.Response.Register(response.CORSHeaders())
s.Pipeline.Response.Register(response.Cache(300), maniflex.ForOperation(maniflex.OpRead, maniflex.OpList), maniflex.AtPosition(maniflex.After))
s.Pipeline.Response.Register(response.TransformField("avatar_url", func(v any) any { return cdnBase + v.(string) }))
s.Pipeline.Response.Register(response.RedactField("phone", func(ctx *maniflex.ServerContext) bool { return !ctx.HasRole("support") }))
s.Pipeline.Response.Register(response.Envelope(func(ctx *maniflex.ServerContext, data any, meta \*maniflex.ResponseMeta) any { return map[string]any{"result": data} }))
s.Pipeline.Response.Register(response.AddHeader("Strict-Transport-Security", "max-age=63072000"))
s.Pipeline.Response.Register(response.Logging(slog.Default()), maniflex.AtPosition(maniflex.After))
s.Pipeline.Response.Register(response.Metrics(myCollector), maniflex.AtPosition(maniflex.After))

// ── OpenAPI ───────────────────────────────────────────────────────────────────
s.Pipeline.OpenAPI.Generate.Register(openapi.AddSecurityScheme("bearerAuth", maniflex.OASSecurityScheme{Type: "http", Scheme: "bearer"}), maniflex.After)
s.Pipeline.OpenAPI.Generate.Register(openapi.AddServer("https://api.example.com", "Production"), maniflex.After)
s.Pipeline.OpenAPI.Generate.Register(openapi.SetTitle("My API"), maniflex.After)
s.Pipeline.OpenAPI.Generate.Register(openapi.SetDescription("# My API\nDocs here."), maniflex.After)
s.Pipeline.OpenAPI.Generate.Register(openapi.AddExtension(func(spec _maniflex.OpenAPISpec) { /_ mutate freely \*/ }), maniflex.After)
```
