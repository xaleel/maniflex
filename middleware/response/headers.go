// Package response provides router-level CORS handling plus Response-step
// middleware for caching, field transforms, custom envelopes, and observability.
package response

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/xaleel/maniflex"
)

// ── CORSHeaders ───────────────────────────────────────────────────────────────

// CORSConfig configures CORS header behaviour.
type CORSConfig struct {
	// AllowOrigins is the list of allowed origins. It is REQUIRED: construction
	// panics if it is empty, so a permissive wildcard is never applied by
	// accident. Use ["*"] to explicitly allow any origin (public APIs only — it
	// cannot be combined with AllowCredentials).
	AllowOrigins []string
	// AllowHeaders are the headers a browser may send. Default: common safe set.
	AllowHeaders []string
	// AllowMethods are the HTTP methods to allow.
	// Default: GET, HEAD, POST, PATCH, DELETE, OPTIONS.
	AllowMethods []string
	// ExposeHeaders are response headers browser JavaScript may read.
	// Default: ETag and X-Request-ID.
	ExposeHeaders []string
	// MaxAge is the preflight cache duration in seconds. Default: 86400 (24h).
	MaxAge int
	// AllowCredentials sets Access-Control-Allow-Credentials. Default: false.
	// It cannot be combined with a "*" wildcard origin (the Fetch spec forbids
	// that combination); doing so panics at construction.
	AllowCredentials bool
}

var defaultAllowHeaders = []string{
	"Authorization", "Content-Type", "Accept",
	"If-Match", "If-None-Match", "X-Request-ID", "X-API-Key",
}
var defaultAllowMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch,
	http.MethodDelete, http.MethodOptions,
}
var defaultExposeHeaders = []string{"ETag", "X-Request-ID"}

// CORSHeaders returns router-level CORS middleware. Add it to
// Config.HTTPMiddlewares; do not register it on a pipeline step. Router-level
// placement lets valid browser preflight requests finish before Auth runs.
//
// At least one origin is required — pass explicit origins (recommended) or "*"
// to allow any origin. Calling it with no origins panics, so a permissive
// wildcard is never applied by accident (SEC-6). For credentials or custom
// allowed headers/methods/max-age, use CORSHeadersWithConfig.
//
//	cfg.HTTPMiddlewares = append(cfg.HTTPMiddlewares,
//	    response.CORSHeaders("https://myapp.com"),
//	)
func CORSHeaders(allowedOrigins ...string) maniflex.HTTPMiddleware {
	return CORSHeadersWithConfig(CORSConfig{AllowOrigins: allowedOrigins})
}

// CORSHeadersWithConfig is CORSHeaders with full control over allowed headers,
// methods, exposed headers, preflight max-age, and credentials. It explicitly
// validates true preflight requests and returns 204 with no body when allowed.
// cfg.AllowOrigins is required (empty panics), and a "*" origin combined with
// AllowCredentials panics because browsers reject that combination.
//
//	cfg.HTTPMiddlewares = append(cfg.HTTPMiddlewares,
//	    response.CORSHeadersWithConfig(response.CORSConfig{
//	        AllowOrigins:     []string{"https://myapp.com"},
//	        AllowCredentials: true,
//	    }),
//	)
func CORSHeadersWithConfig(cfg CORSConfig) maniflex.HTTPMiddleware {
	c := cfg
	if len(c.AllowOrigins) == 0 {
		panic(`response.CORSHeaders: at least one allowed origin is required ` +
			`(pass explicit origins, or "*" to allow any origin)`)
	}
	if len(c.AllowHeaders) == 0 {
		c.AllowHeaders = append([]string(nil), defaultAllowHeaders...)
	}
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = append([]string(nil), defaultAllowMethods...)
	}
	if len(c.ExposeHeaders) == 0 {
		c.ExposeHeaders = append([]string(nil), defaultExposeHeaders...)
	}
	if c.MaxAge == 0 {
		c.MaxAge = 86400
	}
	if c.MaxAge < 0 {
		panic("response.CORSHeaders: MaxAge must not be negative")
	}

	originSet := make(map[string]struct{}, len(c.AllowOrigins))
	for _, raw := range c.AllowOrigins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			panic("response.CORSHeaders: allowed origins must not contain an empty value")
		}
		originSet[origin] = struct{}{}
	}
	_, allowAll := originSet["*"]

	if allowAll && c.AllowCredentials {
		panic(`response.CORSHeaders: AllowCredentials cannot be combined with a ` +
			`"*" wildcard origin (browsers reject it); list explicit origins instead`)
	}

	methods, methodSet := normalizeCORSValues("method", c.AllowMethods, strings.ToUpper)
	headers, headerSet := normalizeCORSValues("header", c.AllowHeaders, canonicalCORSHeader)
	exposed, _ := normalizeCORSValues("exposed header", c.ExposeHeaders, canonicalCORSHeader)
	methodVal := strings.Join(methods, ", ")
	headerVal := strings.Join(headers, ", ")
	exposeVal := strings.Join(exposed, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reqID := chiMiddleware.GetReqID(r.Context()); reqID != "" {
				w.Header().Set("X-Request-Id", reqID)
			}

			origin := r.Header.Get("Origin")
			requestedMethod := strings.TrimSpace(r.Header.Get("Access-Control-Request-Method"))
			preflight := r.Method == http.MethodOptions && origin != "" && requestedMethod != ""
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !allowAll {
				addVary(w.Header(), "Origin")
			}
			if preflight {
				addVary(w.Header(), "Access-Control-Request-Method")
				addVary(w.Header(), "Access-Control-Request-Headers")
			}

			if _, allowed := originSet[origin]; !allowAll && !allowed {
				if preflight {
					writeCORSFailure(w, http.StatusForbidden, "CORS_ORIGIN_DENIED")
					return
				}
				// CORS is a browser response policy, not request authentication.
				// For an ordinary request, omit approval and let the route decide
				// the HTTP result; the browser will keep it from the caller.
				next.ServeHTTP(w, r)
				return
			}

			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			if c.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if exposeVal != "" {
				w.Header().Set("Access-Control-Expose-Headers", exposeVal)
			}

			if !preflight {
				next.ServeHTTP(w, r)
				return
			}

			method := strings.ToUpper(requestedMethod)
			if _, allowed := methodSet[strings.ToLower(method)]; !allowed {
				w.Header().Set("Allow", methodVal)
				writeCORSFailure(w, http.StatusMethodNotAllowed, "CORS_METHOD_DENIED")
				return
			}
			for _, requestedHeader := range requestedCORSHeaders(r.Header) {
				if _, allowed := headerSet[strings.ToLower(requestedHeader)]; !allowed {
					writeCORSFailure(w, http.StatusForbidden, "CORS_HEADER_DENIED")
					return
				}
			}

			w.Header().Set("Access-Control-Allow-Headers", headerVal)
			w.Header().Set("Access-Control-Allow-Methods", methodVal)
			w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", c.MaxAge))
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

func normalizeCORSValues(kind string, values []string, canonical func(string) string) ([]string, map[string]struct{}) {
	normalized := make([]string, 0, len(values))
	set := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := canonical(strings.TrimSpace(raw))
		if value == "" || !isHTTPToken(value) {
			panic(fmt.Sprintf("response.CORSHeaders: invalid %s %q", kind, raw))
		}
		key := strings.ToLower(value)
		if _, duplicate := set[key]; duplicate {
			continue
		}
		set[key] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, set
}

func canonicalCORSHeader(value string) string {
	if strings.EqualFold(value, "ETag") {
		return "ETag"
	}
	return http.CanonicalHeaderKey(value)
}

func requestedCORSHeaders(header http.Header) []string {
	var requested []string
	for _, line := range header.Values("Access-Control-Request-Headers") {
		for _, value := range strings.Split(line, ",") {
			value = strings.TrimSpace(value)
			if value != "" {
				requested = append(requested, value)
			}
		}
	}
	return requested
}

func isHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r > 127 || r <= 32 || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r) {
			return false
		}
	}
	return true
}

func addVary(header http.Header, value string) {
	if value == "*" {
		header.Set("Vary", "*")
		return
	}
	for _, line := range header.Values("Vary") {
		for _, existing := range strings.Split(line, ",") {
			if strings.TrimSpace(existing) == "*" {
				return
			}
			if strings.EqualFold(strings.TrimSpace(existing), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

func writeCORSFailure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": strings.ToLower(http.StatusText(status)),
		},
	})
}

// ── Cache ─────────────────────────────────────────────────────────────────────

// CacheConfig defines who may store a response and which request headers form
// its cache key.
type CacheConfig struct {
	// Public explicitly permits shared caches (CDNs and proxies) to store the
	// response. This is never inferred. Do not enable it for authenticated,
	// tenant-scoped, or dynamically redacted data unless every authorization
	// input is represented in Vary and honored by the shared cache.
	Public bool

	// Private permits only a user-agent cache to store the response. It is the
	// default when Public and NoStore are false, so the zero config is safe for
	// authenticated APIs. Setting it explicitly can make intent clearer.
	Private bool

	// NoStore forbids all caches from storing the response. It cannot be
	// combined with Public, Private, or a non-zero MaxAge. NoStore responses do
	// not receive an ETag and do not honor If-None-Match.
	NoStore bool

	// MaxAge is the freshness lifetime in seconds. Zero means immediately stale
	// and still permits validation with ETag unless NoStore is true.
	MaxAge int

	// Vary names request headers that affect the representation. Values are
	// canonicalized, deduplicated, and merged with existing Vary entries.
	Vary []string
}

// Cache sets an explicit Cache-Control policy on read and list responses and
// adds ETag validation to successful, storable responses. The zero CacheConfig
// is private with max-age=0. Shared/public caching always requires Public: true.
//
// It is a no-op on write operations (create, update, delete).
//
//	server.Pipeline.Response.Register(
//	    response.Cache(response.CacheConfig{
//	        MaxAge: 300, // private, 5 minutes
//	        Vary:   []string{"Authorization"},
//	    }),
//	    maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Cache(cfg CacheConfig) maniflex.MiddlewareFunc {
	if cfg.MaxAge < 0 {
		panic("response.Cache: MaxAge must not be negative")
	}
	policies := 0
	for _, selected := range []bool{cfg.Public, cfg.Private, cfg.NoStore} {
		if selected {
			policies++
		}
	}
	if policies > 1 {
		panic("response.Cache: Public, Private, and NoStore are mutually exclusive")
	}
	if cfg.NoStore && cfg.MaxAge != 0 {
		panic("response.Cache: NoStore cannot be combined with MaxAge")
	}
	vary := normalizeVary(cfg.Vary)

	cacheControl := fmt.Sprintf("private, max-age=%d", cfg.MaxAge)
	if cfg.Public {
		cacheControl = fmt.Sprintf("public, max-age=%d", cfg.MaxAge)
	} else if cfg.NoStore {
		cacheControl = "no-store"
	}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Operation != maniflex.OpRead && ctx.Operation != maniflex.OpList {
			return nil
		}
		if ctx.Response == nil {
			return nil
		}
		ctx.Writer.Header().Set("Cache-Control", cacheControl)
		for _, value := range vary {
			addVary(ctx.Writer.Header(), value)
		}
		if cfg.NoStore {
			return nil
		}

		status := ctx.Response.StatusCode
		if status != 0 && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
			return nil
		}

		// Compute a lightweight ETag from the response data
		if ctx.Response.Data != nil {
			b, err := json.Marshal(ctx.Response.Data)
			if err == nil {
				etag := fmt.Sprintf(`"%x"`, md5.Sum(b))
				ctx.Writer.Header().Set("ETag", etag)

				// Honour If-None-Match
				if ctx.Request.Header.Get("If-None-Match") == etag {
					ctx.Response.StatusCode = http.StatusNotModified
					ctx.Response.Data = nil
				}
			}
		}
		return nil
	}
}

func normalizeVary(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	hasWildcard := false
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "*" {
			hasWildcard = true
		} else {
			value = canonicalCORSHeader(value)
			if value == "" || !isHTTPToken(value) {
				panic(fmt.Sprintf("response.Cache: invalid Vary header %q", raw))
			}
		}
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, value)
	}
	if hasWildcard && len(normalized) != 1 {
		panic(`response.Cache: Vary "*" cannot be combined with other header names`)
	}
	return normalized
}

// ── TransformField ────────────────────────────────────────────────────────────

// TransformFunc maps one field value to another in the response.
type TransformFunc func(value any) any

// TransformField applies fn to the named field in every item of the response
// data. Works on both single-record and list responses.
//
//	// Prefix avatar URLs with a CDN base
//	server.Pipeline.Response.Register(
//	    response.TransformField("avatar_url", func(v any) any {
//	        if s, ok := v.(string); ok && s != "" {
//	            return "https://cdn.example.com/" + s
//	        }
//	        return v
//	    }),
//	    maniflex.ForModel("User"),
//	    maniflex.AtPosition(maniflex.After),
//	)
func TransformField(field string, fn TransformFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Response == nil {
			return nil
		}
		ctx.Response.Data = applyTransform(ctx.Response.Data, field, fn)
		return nil
	}
}

func applyTransform(data any, field string, fn TransformFunc) any {
	switch d := data.(type) {
	case map[string]any:
		if v, ok := d[field]; ok {
			d[field] = fn(v)
		}
		return d
	case []any:
		for i, item := range d {
			d[i] = applyTransform(item, field, fn)
		}
		return d
	}
	return data
}

// ── RedactField ───────────────────────────────────────────────────────────────

// RedactField removes a field from the response dynamically, based on a
// condition. Unlike the static mfx:"hidden" tag this can check ctx.Auth.
//
//	// Hide phone unless the caller has the "support" role
//	server.Pipeline.Response.Register(
//	    response.RedactField("phone", func(ctx *maniflex.ServerContext) bool {
//	        return !ctx.HasRole("support")
//	    }),
//	    maniflex.ForModel("User"),
//	    maniflex.AtPosition(maniflex.After),
//	)
func RedactField(field string, shouldRedact func(ctx *maniflex.ServerContext) bool) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		// Declared before next(), not after: an export streams its bytes during
		// next() and never builds a ctx.Response, so a middleware that only
		// mutated the response masked the JSON and left the CSV in full (audit
		// MS-11). The declaration is what the export honours.
		if shouldRedact(ctx) {
			ctx.RedactResponseField(field)
		}
		if err := next(); err != nil {
			return err
		}
		if ctx.Response == nil || !shouldRedact(ctx) {
			return nil
		}
		// Belt and braces: the marshalling paths already drop a declared field,
		// but a Replace middleware may have built ctx.Response.Data by hand.
		ctx.Response.Data = applyRedact(ctx.Response.Data, field)
		return nil
	}
}

func applyRedact(data any, field string) any {
	switch d := data.(type) {
	case map[string]any:
		delete(d, field)
		return d
	case []any:
		for i, item := range d {
			d[i] = applyRedact(item, field)
		}
		return d
	}
	return data
}

// ── Envelope ──────────────────────────────────────────────────────────────────

// EnvelopeFunc wraps the outgoing response data in a custom structure.
// Return a new map to replace the default {"data": ...} envelope.
type EnvelopeFunc func(ctx *maniflex.ServerContext, data any, meta *maniflex.ResponseMeta) any

// Envelope replaces the default {"data": ..., "meta": ...} response structure
// with a custom one. Register it with AtPosition(After) on the Response step.
//
//	server.Pipeline.Response.Register(
//	    response.Envelope(func(ctx *maniflex.ServerContext, data any, meta *maniflex.ResponseMeta) any {
//	        out := map[string]any{"result": data, "ok": true}
//	        if meta != nil {
//	            out["pagination"] = meta
//	        }
//	        return out
//	    }),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Envelope(fn EnvelopeFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Response == nil || ctx.Response.Error != nil {
			return nil
		}
		// Replace Data with the custom envelope; the Response step will still
		// serialise ctx.Response normally, but now Data holds the full envelope.
		wrapped := fn(ctx, ctx.Response.Data, ctx.Response.Meta)
		ctx.Response.Data = wrapped
		ctx.Response.Meta = nil // meta already embedded in the custom envelope
		return nil
	}
}

// ── AddHeader ─────────────────────────────────────────────────────────────────

// AddHeader unconditionally adds an HTTP response header.
//
//	server.Pipeline.Response.Register(
//	    response.AddHeader("X-Powered-By", "maniflex"),
//	    maniflex.AtPosition(maniflex.After),
//	)
func AddHeader(key, value string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		ctx.Writer.Header().Set(key, value)
		return next()
	}
}

// ── Logging ───────────────────────────────────────────────────────────────────

// Logging emits a structured log line after every request using slog.
// Register it with AtPosition(After) on the Response step.
//
//	server.Pipeline.Response.Register(
//	    response.Logging(slog.Default()),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Logging(logger *slog.Logger) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		start := time.Now()
		if err := next(); err != nil {
			return err
		}

		status := 0
		if ctx.Response != nil {
			status = ctx.Response.StatusCode
		}
		actor := ""
		if ctx.Auth != nil {
			actor = ctx.Auth.UserID
		}

		attrs := []slog.Attr{
			slog.String("method", ctx.Request.Method),
			slog.String("path", ctx.Request.URL.Path),
			slog.String("model", ctx.Model.Name),
			slog.String("op", string(ctx.Operation)),
			slog.Int("status", status),
			// TODO capture duration more accurately (before 1st step to after last)
			slog.Duration("duration", time.Since(start)),
		}
		if ctx.RequestID != "" {
			attrs = append(attrs, slog.String("request_id", ctx.RequestID))
		}
		if actor != "" {
			attrs = append(attrs, slog.String("actor", actor))
		}
		if ctx.ResourceID != "" {
			attrs = append(attrs, slog.String("resource_id", ctx.ResourceID))
		}

		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}

		logger.LogAttrs(context.Background(), level, "request", attrs...)
		return nil
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

// MetricsCollector is satisfied by any metrics library.
// Implement it to bridge to Prometheus, Datadog, StatsD, etc.
type MetricsCollector interface {
	// IncCounter increments a counter with the given labels.
	IncCounter(name string, labels map[string]string)
	// ObserveHistogram records a duration observation.
	ObserveHistogram(name string, value float64, labels map[string]string)
}

// Metrics records request counters and latency histograms.
// Register it with AtPosition(After) on the Response step.
//
//	server.Pipeline.Response.Register(
//	    response.Metrics(myCollector),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Metrics(collector MetricsCollector) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		start := time.Now()
		if err := next(); err != nil {
			return err
		}

		status := "0"
		if ctx.Response != nil {
			status = fmt.Sprintf("%d", ctx.Response.StatusCode)
		}
		labels := map[string]string{
			"model":     ctx.Model.Name,
			"operation": string(ctx.Operation),
			"status":    status,
		}

		collector.IncCounter("maniflex_requests_total", labels)
		collector.ObserveHistogram("maniflex_request_duration_seconds",
			time.Since(start).Seconds(), labels)

		return nil
	}
}
