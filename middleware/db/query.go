// Package db provides DB-step middleware for query control, tenancy, rate
// limiting, auditing, and cache invalidation.
package db

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xaleel/maniflex"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func unionKeys(a, b map[string]any) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// ── ForceFilter ───────────────────────────────────────────────────────────────

// ForceFilterFunc derives the forced filter value from the request context.
type ForceFilterFunc func(ctx *maniflex.ServerContext) any

// ForceFilter injects a FilterExpr into ctx.Query.Filters before the DB step
// runs, regardless of what the client sent. This is the canonical building
// block for multi-tenancy, row-level security, and ownership scoping.
//
//	// Restrict every list/read to the authenticated user's records
//	server.Pipeline.DB.Register(
//	    db.ForceFilter("user_id", func(ctx *maniflex.ServerContext) any {
//	        if ctx.Auth != nil { return ctx.Auth.UserID }
//	        return ""
//	    }),
//	    maniflex.ProvidesScope(),
//	)
//
// Add maniflex.ProvidesScope() so the filter is in place before Validate runs,
// not just before the DB step. Without it the scope still applies to the query,
// but anything earlier in the chain that needs to know which rows the caller can
// see cannot ask — which for a scoped Singleton means its row id is unknown
// during validation, and validate.UniqueField reports the row as conflicting
// with itself (audit 13.12).
func ForceFilter(field string, fn ForceFilterFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		val := fn(ctx)
		if val == nil {
			return next()
		}

		// On writes: stamp the scope onto the row, as Tenancy does and as
		// ActionScope and scoped-singleton provisioning both do via
		// scopeColumns. An equality on a plain column says two things at once —
		// "only rows where field = value" and "a row you create has field =
		// value" — and ForceFilter used to honour only the first. A create was
		// stored with whatever the client sent in the scope column, so the row
		// landed outside its own author's scope: invisible to them on their very
		// next read, and, because that column is the client's to send, plantable
		// straight into someone else's scope (audit 13.8). Updates are stamped
		// too: the forced filter stops an update *reaching* a row outside scope,
		// but not one rewriting the column to push a row out of it.
		//
		// An immutable scope column is left alone on update — it cannot change,
		// so re-sending it would only produce a redundant write or a rejection.
		if ctx.Operation == maniflex.OpCreate ||
			(ctx.Operation == maniflex.OpUpdate && !scopeFieldImmutable(ctx, field)) {
			ctx.SetField(field, val)
		}

		if ctx.Query == nil {
			ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
		}
		ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
			Field:    field,
			Operator: maniflex.OpEq,
			Value:    val,
			Forced:   true, // scopes updates and deletes too, not just reads
		})
		return next()
	}
}

// ForceFilterVia scopes a model that has no column to scope by, through the
// column its BelongsTo parent carries.
//
// ForceFilter maps a field to a value, which needs the model to hold that field.
// A child table often does not: a DamagedItem has an item_id and nothing else,
// and whether it is yours is a fact about its Item. The alternatives are to
// denormalise owner_id onto every such child — a schema change, plus a standing
// obligation to keep it in step, on exactly the tables whose scoping is easiest
// to get wrong — or to hand-write the predicate, which is what declarative
// scoping exists to replace.
//
//	// A DamagedItem is the caller's if its Item is
//	server.Pipeline.DB.Register(
//	    db.ForceFilterVia("item", "owner_id", func(ctx *maniflex.ServerContext) any {
//	        return ctx.Auth.Claims["owner_id"]
//	    }),
//	    maniflex.ForModel("DamagedItem"),
//	)
//
// relationKey names the relation in the vocabulary a nested ?filter= uses
// (?filter=author.status:neq:banned → "author"); parentField names the column on
// the parent, by JSON or DB name. Reads join the parent and apply the predicate.
// Updates and deletes read the row back through it and answer 404 on a miss, as
// they do for ForceFilter. A create — and an update that rewrites the foreign key
// — is refused unless the parent it names is itself in scope, because that key is
// what the scope hangs from and the client is the one supplying it.
//
// Like ForceFilter, a fn returning nil applies no filter, so a resolver that
// cannot identify the caller leaves the request unscoped: return a value that
// matches nothing if that is not what you want, or use maniflex.ForOperation to
// say where the scope applies. Unlike ForceFilter, the relation is resolved per
// request against the model being served, so registering it on a model without
// that relation fails the request rather than serving it unscoped.
func ForceFilterVia(relationKey, parentField string, fn ForceFilterFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		val := fn(ctx)
		if val == nil {
			return next()
		}
		f, err := ctx.ViaFilter(relationKey, parentField, val)
		if err != nil {
			// Fail closed. A scope that cannot be built is not a weaker scope, it is
			// no scope, and next() here would serve the whole table.
			return err
		}
		if ctx.Query == nil {
			ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
		}
		ctx.Query.Filters = append(ctx.Query.Filters, f)
		return next()
	}
}

// ── Tenancy ───────────────────────────────────────────────────────────────────

// TenantFunc extracts the tenant identifier from the request context.
type TenantFunc func(ctx *maniflex.ServerContext) string

// Tenancy is a higher-level multi-tenancy middleware that combines ForceFilter
// (on reads) with automatic field injection (on writes). It ensures every
// record is scoped to the tenant and that tenants cannot read each other's data.
//
//	server.Pipeline.DB.Register(
//	    db.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
//	        if ctx.Auth != nil {
//	            if orgID, _ := ctx.Auth.Claims["org_id"].(string); orgID != "" {
//	                return orgID
//	            }
//	        }
//	        return ""
//	    }),
//	)
func Tenancy(tenantField string, fn TenantFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		tenantID := fn(ctx)
		if tenantID == "" {
			ctx.Abort(http.StatusForbidden, "FORBIDDEN", "tenant identity could not be determined")
			return nil
		}

		// On writes: inject the tenant field into the body — but skip
		// OpUpdate when the field is marked immutable. The Validate step
		// strips immutable fields from PATCH bodies before this middleware
		// runs, so injecting it again would either undo that protection or
		// produce a redundant write (depending on adapter behaviour).
		//
		// Note this injection is why an unscoped cross-tenant update was not
		// merely a leak: it stamped the caller's tenant onto a row that was not
		// theirs, handing them the record and leaving the owner to 404 on it. The
		// forced filter below is what stops the write reaching that row at all.
		if ctx.Operation == maniflex.OpCreate ||
			(ctx.Operation == maniflex.OpUpdate && !scopeFieldImmutable(ctx, tenantField)) {
			// Write through to both ParsedBody and the typed record (ctx.Record).
			ctx.SetField(tenantField, tenantID)
		}

		// On all operations: enforce the filter. Forced marks it as a scope the
		// server imposed, which is what carries it onto updates and deletes — a
		// plain filter only ever constrained reads.
		if ctx.Query == nil {
			ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
		}
		ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
			Field:    tenantField,
			Operator: maniflex.OpEq,
			Value:    tenantID,
			Forced:   true,
		})

		return next()
	}
}

// scopeFieldImmutable reports whether the model's scope column — Tenancy's
// tenant field or ForceFilter's field — carries the `mfx:"immutable"` tag.
// Returns false when the model or field cannot be resolved, in which case both
// keep the historical behaviour of injecting the value.
func scopeFieldImmutable(ctx *maniflex.ServerContext, scopeField string) bool {
	if ctx.Model == nil {
		return false
	}
	if f := ctx.Model.FieldByJSONName(scopeField); f != nil {
		return f.Tags.Immutable
	}
	// scopeField is sometimes supplied as a DB column name rather than a
	// JSON name (e.g. "tenant_id"). Walk the field list as a fallback.
	for _, fm := range ctx.Model.Fields {
		if fm.Tags.DBName == scopeField {
			return fm.Tags.Immutable
		}
	}
	return false
}

// ── Paginate ──────────────────────────────────────────────────────────────────

// Paginate clamps ctx.Query.Limit to a model-specific maximum, overriding the
// global maxLimit constant. Use this on sensitive or large models where you
// never want more than N rows returned regardless of the client's request.
//
//	server.Pipeline.DB.Register(
//	    db.Paginate(50),
//	    maniflex.ForModel("AuditLog"),
//	)
func Paginate(maxLimit int) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Query != nil && ctx.Query.Limit > maxLimit {
			ctx.Query.Limit = maxLimit
		}
		return next()
	}
}

// ── RateLimit ─────────────────────────────────────────────────────────────────

// RateLimitConfig controls the rate limiter.
type RateLimitConfig struct {
	// RequestsPerMinute is the number of requests allowed per key per minute.
	// When Window is also set, this field is treated as the request count for
	// that window (the name is kept for backward compatibility).
	RequestsPerMinute int
	// Window is the sliding-window duration. When zero, defaults to one minute.
	Window time.Duration
	// KeyFunc derives the rate-limit key from the request.
	// Default: ctx.Auth.UserID, falling back to the remote IP.
	KeyFunc func(ctx *maniflex.ServerContext) string
	// ErrorMessage is the 429 response message. Default: "rate limit exceeded".
	ErrorMessage string
	// Backend, when non-nil, replaces the default in-process sync.Map counter.
	// Use a shared backend (e.g. Redis) so multiple replicas enforce one
	// rate-limit window. See middleware/db/redis for the Redis implementation.
	Backend RateLimitBackend
}

// RateLimitBackend is the pluggable counter behind RateLimit. Implementations
// must atomically increment the counter for key and return the new value;
// they are also responsible for expiring the counter after window elapses
// (so the next request after the window resets the count).
type RateLimitBackend interface {
	Increment(ctx context.Context, key string, window time.Duration) (int64, error)
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
	// inserts counts new-key additions; once it reaches pruneEvery the next
	// acquire triggers a full sweep that removes expired windows. This caps
	// memory for long-tail keys (per-IP, per-email limits) without needing
	// a background goroutine that would leak in test setups where many
	// RateLimit middlewares are constructed.
	inserts int
}

const rateLimiterPruneEvery = 128

type window struct {
	count   int
	resetAt time.Time
}

// RateLimit is an in-process token-bucket rate limiter keyed on the
// authenticated user ID (or remote IP for unauthenticated requests).
// For a limit shared across replicas, set RateLimitConfig.Backend to a
// distributed counter (see middleware/db/redis for the Redis implementation).
//
//	// Limit writes to 60/minute globally
//	server.Pipeline.DB.Register(
//	    db.RateLimit(db.RateLimitConfig{RequestsPerMinute: 60}),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	)
//
//	// Tighter limit per model
//	server.Pipeline.DB.Register(
//	    db.RateLimit(db.RateLimitConfig{RequestsPerMinute: 10}),
//	    maniflex.ForModel("PasswordReset"),
//	)
func RateLimit(cfg RateLimitConfig) maniflex.MiddlewareFunc {
	if cfg.RequestsPerMinute <= 0 {
		cfg.RequestsPerMinute = 60
	}
	if cfg.ErrorMessage == "" {
		cfg.ErrorMessage = "rate limit exceeded — try again later"
	}
	keyFn := cfg.KeyFunc
	if keyFn == nil {
		keyFn = func(ctx *maniflex.ServerContext) string {
			if ctx.Auth != nil && ctx.Auth.UserID != "" {
				return ctx.Auth.UserID
			}
			return ctx.Request.RemoteAddr
		}
	}

	windowDur := cfg.Window
	if windowDur <= 0 {
		windowDur = time.Minute
	}
	retryAfter := fmt.Sprintf("%d", int(windowDur.Seconds()))

	limiter := &rateLimiter{windows: make(map[string]*window)}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		key := fmt.Sprintf("%s:%s:%s", keyFn(ctx), ctx.Model.TableName, ctx.Operation)

		over, err := rateLimitCheck(ctx, cfg, limiter, key, windowDur)
		if err != nil {
			return err
		}
		if over {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusTooManyRequests,
				Error: &maniflex.APIError{
					Code:    "RATE_LIMITED",
					Message: cfg.ErrorMessage,
				},
			}
			ctx.Writer.Header().Set("Retry-After", retryAfter)
			return nil
		}
		return next()
	}
}

func rateLimitCheck(ctx *maniflex.ServerContext, cfg RateLimitConfig, limiter *rateLimiter, key string, windowDur time.Duration) (bool, error) {
	if cfg.Backend != nil {
		count, err := cfg.Backend.Increment(ctx.Request.Context(), key, windowDur)
		if err != nil {
			return false, fmt.Errorf("ratelimit backend: %w", err)
		}
		return count > int64(cfg.RequestsPerMinute), nil
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := time.Now()
	w, ok := limiter.windows[key]
	if !ok || now.After(w.resetAt) {
		w = &window{resetAt: now.Add(windowDur)}
		limiter.windows[key] = w
		limiter.inserts++
		if limiter.inserts >= rateLimiterPruneEvery {
			limiter.inserts = 0
			for k, v := range limiter.windows {
				if now.After(v.resetAt) {
					delete(limiter.windows, k)
				}
			}
		}
	}
	w.count++
	return w.count > cfg.RequestsPerMinute, nil
}

// RateLimitFieldOption configures RateLimitField.
type RateLimitFieldOption func(*RateLimitConfig)

// WithRateLimitBackend sets a shared distributed backend on a RateLimitField.
func WithRateLimitBackend(b RateLimitBackend) RateLimitFieldOption {
	return func(c *RateLimitConfig) { c.Backend = b }
}

// WithRateLimitErrorMessage overrides the default 429 error message on a
// RateLimitField.
func WithRateLimitErrorMessage(msg string) RateLimitFieldOption {
	return func(c *RateLimitConfig) { c.ErrorMessage = msg }
}

// RateLimitField rate-limits requests by the value of a parsed request-body
// field rather than by request identity. Common use: cap password-reset or
// OTP attempts per email address.
//
//	server.Pipeline.DB.Register(
//	    db.RateLimitField("email", 3, time.Hour),
//	    maniflex.ForModel("PasswordReset"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
//
// If the field is absent from the parsed body the rate limit is not applied —
// legitimate requests are never blocked due to a missing field. Use
// WithRateLimitBackend to share the window across replicas.
func RateLimitField(field string, limit int, window time.Duration, opts ...RateLimitFieldOption) maniflex.MiddlewareFunc {
	cfg := RateLimitConfig{
		RequestsPerMinute: limit,
		Window:            window,
	}
	for _, o := range opts {
		o(&cfg)
	}
	cfg.KeyFunc = func(ctx *maniflex.ServerContext) string {
		if ctx.ParsedBody == nil {
			return ""
		}
		v, ok := ctx.ParsedBody.Get(field)
		if !ok || v == nil {
			return ""
		}
		// Namespace by field name so different fields with the same value on
		// the same model never share a bucket. Model and operation are appended
		// by RateLimit's own key builder.
		return fmt.Sprintf("field:%s:%v", field, v)
	}
	inner := RateLimit(cfg)
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		v, ok := ctx.ParsedBody.Get(field)
		if !ok || v == nil {
			return next()
		}
		return inner(ctx, next)
	}
}

// ── AuditLog ──────────────────────────────────────────────────────────────────

// FieldChange records the before and after value of a single field.
// For create operations From is nil; for delete operations To is nil.
type FieldChange struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// AuditRecord is the structured log entry written for every audited operation.
type AuditRecord struct {
	Timestamp   time.Time              `json:"timestamp"`
	Model       string                 `json:"model"`
	Operation   maniflex.Operation     `json:"operation"`
	ResourceID  string                 `json:"resource_id,omitempty"`
	Actor       string                 `json:"actor,omitempty"`
	TenantID    string                 `json:"tenant_id,omitempty"`
	RequestID   string                 `json:"request_id,omitempty"`
	TraceID     string                 `json:"trace_id,omitempty"`
	ServiceName string                 `json:"service_name,omitempty"`
	Result      any                    `json:"result,omitempty"`
	Changes     map[string]FieldChange `json:"changes,omitempty"`
}

// AuditSink receives audit records. Implement this interface to write to a
// database table, structured logger, or external audit service.
type AuditSink interface {
	Write(ctx context.Context, record AuditRecord) error
}

// AuditLogOption configures the AuditLog middleware.
type AuditLogOption func(*auditLogOptions)

type auditLogOptions struct {
	trackChanges  bool
	excludeFields map[string]struct{}
}

// WithChanges enables before/after field diffing on update and delete
// operations, and records all new fields on create.
//
// When this option is set the middleware must be registered at the default
// maniflex.Before position (not AtPosition(After)), because it needs to read the
// record state before the DB step executes the write:
//
//	server.Pipeline.DB.Register(
//	    db.AuditLog(sink, db.WithChanges()),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    // no AtPosition — defaults to Before
//	)
func WithChanges() AuditLogOption {
	return func(o *auditLogOptions) { o.trackChanges = true }
}

// WithExcludeFields prevents the named fields from appearing in the Changes
// diff. Use this to keep encrypted digests, internal tokens, and other
// sensitive columns out of the audit trail.
func WithExcludeFields(fields ...string) AuditLogOption {
	return func(o *auditLogOptions) {
		if o.excludeFields == nil {
			o.excludeFields = make(map[string]struct{}, len(fields))
		}
		for _, f := range fields {
			o.excludeFields[f] = struct{}{}
		}
	}
}

// AuditLog writes a structured audit record after every successful DB write.
//
// Without options, register it with AtPosition(After) so it fires only on
// successful writes:
//
//	server.Pipeline.DB.Register(
//	    db.AuditLog(myAuditSink),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.AtPosition(maniflex.After),
//	)
//
// With WithChanges(), register at the default position (Before) so the
// middleware can read the record state before the write executes:
//
//	server.Pipeline.DB.Register(
//	    db.AuditLog(myAuditSink, db.WithChanges()),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	)
//
// Audit writes are fire-and-forget; sink errors never fail the request.
func AuditLog(sink AuditSink, opts ...AuditLogOption) maniflex.MiddlewareFunc {
	var o auditLogOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		before := auditPreFetch(ctx, o.trackChanges)
		if err := next(); err != nil {
			return err
		}
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}
		record := auditBuildRecord(ctx, before, o)
		ctx.GoBackground(func(bgCtx context.Context) {
			// Cap the write at 5s but inherit Shutdown cancellation from bgCtx
			// so a slow sink doesn't outlive the process.
			writeCtx, cancel := context.WithTimeout(bgCtx, 5*time.Second)
			defer cancel()
			if err := sink.Write(writeCtx, record); err != nil {
				ctx.Logger().Warn("auditlog: sink write failed",
					"model", record.Model,
					"op", string(record.Operation),
					"error", err.Error())
			}
		})
		return nil
	}
}

// auditPreFetch reads the current record before a write so AuditLog can diff
// it. Returns nil for create (no prior state) or when change tracking is off.
func auditPreFetch(ctx *maniflex.ServerContext, trackChanges bool) map[string]any {
	if !trackChanges {
		return nil
	}
	if ctx.Operation != maniflex.OpUpdate && ctx.Operation != maniflex.OpDelete {
		return nil
	}
	// Not ctx.ResourceID: for a scoped Singleton that is still the SingletonID
	// placeholder at this point in the pipeline, and reading through it misses,
	// leaving every field of the result looking newly set (audit 13.9).
	id := ctx.ResolveResourceID()
	if id == "" {
		return nil
	}
	before, _ := ctx.GetModel(ctx.Model.Name).Read(id)
	return before
}

// auditBuildRecord assembles the AuditRecord from the current context.
func auditBuildRecord(ctx *maniflex.ServerContext, before map[string]any, o auditLogOptions) AuditRecord {
	actor, tenantID := "", ""
	if ctx.Auth != nil {
		actor = ctx.Auth.UserID
		tenantID = ctx.Auth.TenantID
	}
	r := AuditRecord{
		Timestamp:   time.Now().UTC(),
		Model:       ctx.Model.Name,
		Operation:   ctx.Operation,
		ResourceID:  ctx.ResourceID,
		Actor:       actor,
		TenantID:    tenantID,
		RequestID:   ctx.RequestID,
		TraceID:     ctx.TraceID,
		ServiceName: ctx.ServiceName(),
		Result:      ctx.DBResult,
	}
	if o.trackChanges {
		r.Changes = computeChanges(ctx.Operation, before, ctx.DBResult, o.excludeFields)
	}
	return r
}

// computeChanges produces a field-level diff between before and after states.
// For create: all fields in the result with From=nil. For update: only changed
// fields. For delete: all fields from before with To=nil.
func computeChanges(op maniflex.Operation, before map[string]any, result any, exclude map[string]struct{}) map[string]FieldChange {
	after, _ := result.(map[string]any)
	var changes map[string]FieldChange
	switch op {
	case maniflex.OpCreate:
		changes = changesForCreate(after, exclude)
	case maniflex.OpUpdate:
		changes = changesForUpdate(before, after, exclude)
	case maniflex.OpDelete:
		changes = changesForDelete(before, exclude)
	}
	return changes
}

func changesForCreate(after map[string]any, exclude map[string]struct{}) map[string]FieldChange {
	out := make(map[string]FieldChange, len(after))
	for k, v := range after {
		if _, skip := exclude[k]; skip {
			continue
		}
		out[k] = FieldChange{To: v}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func changesForUpdate(before, after map[string]any, exclude map[string]struct{}) map[string]FieldChange {
	out := make(map[string]FieldChange)
	for k := range unionKeys(before, after) {
		if _, skip := exclude[k]; skip {
			continue
		}
		b, a := before[k], after[k]
		if fmt.Sprintf("%v", b) != fmt.Sprintf("%v", a) {
			out[k] = FieldChange{From: b, To: a}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func changesForDelete(before map[string]any, exclude map[string]struct{}) map[string]FieldChange {
	out := make(map[string]FieldChange, len(before))
	for k, v := range before {
		if _, skip := exclude[k]; skip {
			continue
		}
		out[k] = FieldChange{From: v}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── Invalidate ────────────────────────────────────────────────────────────────

// CacheKeyFunc derives the cache key(s) to evict from the request context.
type CacheKeyFunc func(ctx *maniflex.ServerContext) []string

// Invalidate evicts one or more cache keys after a successful write operation.
// Register it with AtPosition(After) on the DB step.
//
//	server.Pipeline.DB.Register(
//	    db.Invalidate(redisCache, func(ctx *maniflex.ServerContext) []string {
//	        return []string{
//	            "posts:list",
//	            fmt.Sprintf("posts:%s", ctx.ResourceID),
//	        }
//	    }),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Invalidate(cache maniflex.CacheStore, keyFn CacheKeyFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}

		keys := keyFn(ctx)
		if len(keys) == 0 {
			return nil
		}

		ctx.GoBackground(func(bgCtx context.Context) {
			delCtx, cancel := context.WithTimeout(bgCtx, 3*time.Second)
			defer cancel()
			for _, k := range keys {
				cache.Delete(delCtx, k)
			}
		})

		return nil
	}
}

// ── CacheQuery ──────────────────────────────────────────────────────────────

// CacheConfig configures the CacheQuery middleware.
type CacheConfig struct {
	// TTL is how long a cached result is served before it expires. A
	// non-positive TTL disables caching — every request goes to the database.
	TTL time.Duration

	// KeyFunc derives the cache key from the request. Returning "" skips the
	// cache for that request (no read, no write). The key must capture every
	// input that changes the result — model, tenant, filters, sort, pagination,
	// includes — so two requests share an entry only when their results are
	// identical. ctx.Request.URL.RawQuery (namespaced by model and, for reads,
	// ctx.ResourceID) is a convenient starting point.
	KeyFunc func(ctx *maniflex.ServerContext) string
}

// CacheQuery memoises read results (OpRead, OpList) in a CacheStore — the
// read-side complement to Invalidate. On a cache hit it sets ctx.DBResult and
// the DB step's adapter read is skipped; the Response step then builds the
// envelope from the cached result exactly as it would for a fresh read. On a
// miss it runs the read and stores ctx.DBResult under the key for cfg.TTL.
// Pair it with Invalidate on writes to evict stale entries.
//
//	cache := maniflex.NewMemoryCache() // or a Redis-backed CacheStore
//	key := func(ctx *maniflex.ServerContext) string {
//	    return "products:list:" + ctx.Request.URL.RawQuery
//	}
//	server.Pipeline.DB.Register(
//	    db.CacheQuery(cache, db.CacheConfig{TTL: 5 * time.Minute, KeyFunc: key}),
//	    maniflex.ForModel("Product"),
//	    maniflex.ForOperation(maniflex.OpList, maniflex.OpRead),
//	)
//	server.Pipeline.DB.Register(
//	    db.Invalidate(cache, func(ctx *maniflex.ServerContext) []string {
//	        return []string{"products:list:..."} // keys to evict; see Invalidate
//	    }),
//	    maniflex.ForModel("Product"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.AtPosition(maniflex.After),
//	)
//
// Register it at the default (Before) position. The value stored is
// ctx.DBResult, so a distributed CacheStore must round-trip a
// *maniflex.ListResult for list responses — a store that decodes list entries
// into a bare map yields a logged miss rather than a panic. Avoid caching
// mfx:"encrypted" models: the decrypted result would live in the cache.
func CacheQuery(cache maniflex.CacheStore, cfg CacheConfig) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		key := cacheKey(ctx, cfg)
		if key == "" {
			return next()
		}

		// Cache hit: serve the stored result and skip the adapter read.
		if val, ok := cache.Get(ctx.Ctx, key); ok && cacheHitUsable(ctx, val) {
			ctx.DBResult = val
			return next()
		}

		// Cache miss: run the read, then store the result on success.
		if err := next(); err != nil {
			return err
		}
		if shouldCacheResult(ctx) {
			cache.Set(ctx.Ctx, key, ctx.DBResult, cfg.TTL)
		}
		return nil
	}
}

// cacheKey returns the cache key for this request, or "" when the request must
// not be cached: a non-read operation, a disabled TTL, no KeyFunc, or a KeyFunc
// that opted this request out by returning "".
func cacheKey(ctx *maniflex.ServerContext, cfg CacheConfig) string {
	if cfg.TTL <= 0 || cfg.KeyFunc == nil || !cacheableOp(ctx.Operation) {
		return ""
	}
	return cfg.KeyFunc(ctx)
}

// shouldCacheResult reports whether a freshly-read result is worth storing: it
// must exist and the request must not have ended in an error response.
func shouldCacheResult(ctx *maniflex.ServerContext) bool {
	if ctx.DBResult == nil {
		return false
	}
	return ctx.Response == nil || ctx.Response.StatusCode < 400
}

// cacheableOp reports whether the operation produces a cacheable read result.
func cacheableOp(op maniflex.Operation) bool {
	return op == maniflex.OpRead || op == maniflex.OpList
}

// cacheHitUsable guards against a CacheStore returning a value whose Go type the
// Response step cannot render — e.g. a JSON-decoding store that hands back a bare
// map where a list response expects a *maniflex.ListResult. A list hit must be a
// *ListResult; a read hit may be a typed record or a map, both of which the
// Response step renders. An unusable value is treated as a miss so the request
// falls through to the database instead of panicking.
func cacheHitUsable(ctx *maniflex.ServerContext, val any) bool {
	if val == nil {
		return false
	}
	if ctx.Operation == maniflex.OpList {
		if _, ok := val.(*maniflex.ListResult); !ok {
			ctx.Logger().Warn("db.CacheQuery: ignoring cached list value of unexpected type",
				"model", ctx.Model.Name)
			return false
		}
	}
	return true
}
