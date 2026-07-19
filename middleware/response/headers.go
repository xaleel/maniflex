// Package response provides Response-step middleware for CORS, caching,
// field transforms, custom envelopes, and observability.
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
	// AllowMethods are the HTTP methods to allow. Default: GET, POST, PATCH, DELETE, OPTIONS.
	AllowMethods []string
	// MaxAge is the preflight cache duration in seconds. Default: 86400 (24h).
	MaxAge int
	// AllowCredentials sets Access-Control-Allow-Credentials. Default: false.
	// It cannot be combined with a "*" wildcard origin (the Fetch spec forbids
	// that combination); doing so panics at construction.
	AllowCredentials bool
}

var defaultAllowHeaders = []string{
	"Authorization", "Content-Type", "Accept",
	"X-Request-ID", "X-API-Key",
}
var defaultAllowMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPatch,
	http.MethodDelete, http.MethodOptions,
}

// CORSHeaders sets CORS response headers on every response, and returns 200 for
// OPTIONS preflight requests. Register this on the Response step with AtPosition(After).
//
// At least one origin is required — pass explicit origins (recommended) or "*"
// to allow any origin. Calling it with no origins panics, so a permissive
// wildcard is never applied by accident (SEC-6). For credentials or custom
// allowed headers/methods/max-age, use CORSHeadersWithConfig.
//
//	server.Pipeline.Response.Register(
//	    response.CORSHeaders("https://myapp.com"),
//	    maniflex.AtPosition(maniflex.After),
//	)
func CORSHeaders(allowedOrigins ...string) maniflex.MiddlewareFunc {
	return CORSHeadersWithConfig(CORSConfig{AllowOrigins: allowedOrigins})
}

// CORSHeadersWithConfig is CORSHeaders with full control over allowed headers,
// methods, preflight max-age, and credentials. cfg.AllowOrigins is required
// (empty panics), and a "*" origin combined with AllowCredentials panics because
// browsers reject that combination.
//
//	server.Pipeline.Response.Register(
//	    response.CORSHeadersWithConfig(response.CORSConfig{
//	        AllowOrigins:     []string{"https://myapp.com"},
//	        AllowCredentials: true,
//	    }),
//	    maniflex.AtPosition(maniflex.After),
//	)
func CORSHeadersWithConfig(cfg CORSConfig) maniflex.MiddlewareFunc {
	c := cfg
	if len(c.AllowOrigins) == 0 {
		panic(`response.CORSHeaders: at least one allowed origin is required ` +
			`(pass explicit origins, or "*" to allow any origin)`)
	}
	if len(c.AllowHeaders) == 0 {
		c.AllowHeaders = defaultAllowHeaders
	}
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = defaultAllowMethods
	}
	if c.MaxAge == 0 {
		c.MaxAge = 86400
	}

	originSet := make(map[string]bool, len(c.AllowOrigins))
	for _, o := range c.AllowOrigins {
		originSet[o] = true
	}
	allowAll := originSet["*"]

	if allowAll && c.AllowCredentials {
		panic(`response.CORSHeaders: AllowCredentials cannot be combined with a ` +
			`"*" wildcard origin (browsers reject it); list explicit origins instead`)
	}

	headerVal := strings.Join(c.AllowHeaders, ", ")
	methodVal := strings.Join(c.AllowMethods, ", ")

	return func(ctx *maniflex.ServerContext, next func() error) error {
		origin := ctx.Request.Header.Get("Origin")
		w := ctx.Writer

		if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" && originSet[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}

		w.Header().Set("Access-Control-Allow-Headers", headerVal)
		w.Header().Set("Access-Control-Allow-Methods", methodVal)
		w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", c.MaxAge))
		if c.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// Handle preflight before the pipeline runs
		if ctx.Request.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
			return nil
		}

		return next()
	}
}

// ── Cache ─────────────────────────────────────────────────────────────────────

// Cache sets Cache-Control and ETag headers on read responses.
// It is a no-op on write operations (create, update, delete).
//
//	server.Pipeline.Response.Register(
//	    response.Cache(300), // 5 minutes
//	    maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
//	    maniflex.AtPosition(maniflex.After),
//	)
func Cache(maxAgeSeconds int) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Operation != maniflex.OpRead && ctx.Operation != maniflex.OpList {
			return nil
		}
		if ctx.Response == nil || ctx.Response.StatusCode >= 400 {
			return nil
		}
		ctx.Writer.Header().Set("Cache-Control",
			fmt.Sprintf("public, max-age=%d", maxAgeSeconds))

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
