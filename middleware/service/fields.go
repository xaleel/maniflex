// Package service provides Service-step middleware for common field transforms
// and computed-value injection that runs before the DB write.
package service

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"maniflex"
)

// ── HashField ─────────────────────────────────────────────────────────────────

// Hasher hashes a plaintext field value and returns the encoded hash. HashField
// delegates to it so the hashing algorithm is pluggable and the core service
// package carries no crypto dependency. A bcrypt implementation lives in the
// maniflex/middleware/service/bcrypt satellite.
type Hasher func(plaintext string) (string, error)

// HashField hashes the named field in ctx.ParsedBody with the supplied Hasher
// before the DB write. If the field is absent or nil on a PATCH request the
// field is simply skipped — a user can update other fields without resetting
// their password.
//
//	import svcbcrypt "maniflex/middleware/service/bcrypt"
//
//	server.Pipeline.Service.Register(
//	    service.HashField("password", svcbcrypt.Hasher()),
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
func HashField(field string, h Hasher) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		raw, ok := ctx.Field(field)
		if !ok || raw == nil {
			return next()
		}
		str, ok := raw.(string)
		if !ok || str == "" {
			return next()
		}
		hashed, err := h(str)
		if err != nil {
			ctx.Abort(http.StatusInternalServerError, "HASH_ERROR",
				"failed to hash field: "+field)
			return nil
		}
		ctx.SetField(field, hashed)
		return next()
	}
}

// ── SlugifyField ──────────────────────────────────────────────────────────────

// SlugifyField derives a URL-safe slug from sourceField and writes it to
// destField. If destField is already set in the body it is not overwritten.
//
// The slug is lowercased, spaces/punctuation replaced with hyphens, and
// consecutive hyphens collapsed. A 6-char random suffix is appended if the
// generated slug would be empty.
//
//	server.Pipeline.Service.Register(
//	    service.SlugifyField("title", "slug"),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
func SlugifyField(sourceField, destField string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		// Don't overwrite if already set
		if existing, ok := ctx.Field(destField); ok && existing != nil && existing != "" {
			return next()
		}
		src, ok := ctx.Field(sourceField)
		if !ok || src == nil {
			return next()
		}
		slug := slugify(fmt.Sprintf("%v", src))
		if slug == "" {
			slug = randomSuffix(6)
		}
		ctx.SetField(destField, slug)
		return next()
	}
}

var (
	slugNonAlpha = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim     = regexp.MustCompile(`^-+|-+$`)
)

func slugify(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '-'
	}, s)
	s = slugNonAlpha.ReplaceAllString(s, "-")
	s = slugTrim.ReplaceAllString(s, "")
	return s
}

func randomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i, v := range b {
		b[i] = chars[int(v)%len(chars)]
	}
	return string(b)
}

// ── SetField ──────────────────────────────────────────────────────────────────

// SetFieldFunc is a function that derives the value to inject.
type SetFieldFunc func(ctx *maniflex.ServerContext) any

// SetField sets a field in ctx.ParsedBody to the return value of fn before the
// DB write. The function receives the full ServerContext so it can read ctx.Auth,
// ctx.ResourceID, or other fields.
//
//	// Force author_id to the authenticated user
//	server.Pipeline.Service.Register(
//	    service.SetField("author_id", func(ctx *maniflex.ServerContext) any {
//	        if ctx.Auth != nil { return ctx.Auth.UserID }
//	        return nil
//	    }),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
func SetField(field string, fn SetFieldFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		ctx.SetField(field, fn(ctx))
		return next()
	}
}

// ── StripField ────────────────────────────────────────────────────────────────

// StripField removes a field from ctx.ParsedBody unconditionally before the DB
// write. Use this when a field is accepted by the API for other purposes (e.g.
// a password confirmation field) but must not be persisted.
//
//	server.Pipeline.Service.Register(
//	    service.StripField("password_confirm"),
//	    maniflex.ForModel("User"),
//	)
func StripField(fields ...string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		for _, f := range fields {
			ctx.DeleteField(f)
		}
		return next()
	}
}

// ── CopyField ─────────────────────────────────────────────────────────────────

// CopyField copies the value of sourceField to destField in ctx.ParsedBody.
// If sourceField is absent or nil, nothing happens. If destField is already
// set it is NOT overwritten (use SetField for unconditional assignment).
//
//	// Default username to email on create
//	server.Pipeline.Service.Register(
//	    service.CopyField("email", "username"),
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
func CopyField(sourceField, destField string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if _, alreadySet := ctx.Field(destField); alreadySet {
			return next()
		}
		if v, ok := ctx.Field(sourceField); ok && v != nil {
			ctx.SetField(destField, v)
		}
		return next()
	}
}

// ── Timestamp ─────────────────────────────────────────────────────────────────

// Timestamp sets field to the current UTC time before the DB write.
// Unlike the automatic created_at / updated_at injection in the DB step,
// this targets arbitrary fields (e.g. published_at, verified_at).
//
// The field is only set when it is not already present in ctx.ParsedBody,
// so clients can still supply an explicit value if needed.
//
//	// Set published_at when status becomes "published"
//	server.Pipeline.Service.Register(
//	    service.TimestampWhen("published_at", "status", "published"),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpUpdate),
//	)
func Timestamp(field string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if _, already := ctx.Field(field); !already {
			ctx.SetField(field, time.Now().UTC())
		}
		return next()
	}
}

// TimestampWhen sets field to the current UTC time only when condField equals
// condValue. Useful for recording state-transition timestamps.
func TimestampWhen(field, condField, condValue string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if v, ok := ctx.Field(condField); ok && fmt.Sprintf("%v", v) == condValue {
			if _, already := ctx.Field(field); !already {
				ctx.SetField(field, time.Now().UTC())
			}
		}
		return next()
	}
}

// ── OwnerScope ────────────────────────────────────────────────────────────────

// OwnerScope forces ownerField = ctx.Auth.UserID on create operations, ensuring
// every record is tagged with the authenticated user's ID without relying on the
// client to supply it.
//
//	server.Pipeline.Service.Register(
//	    service.OwnerScope("user_id"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
func OwnerScope(ownerField string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth == nil {
			return next()
		}
		ctx.SetField(ownerField, ctx.Auth.UserID)
		return next()
	}
}
