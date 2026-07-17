package validate

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/xaleel/maniflex"
)

// ── RestrictField / FieldRole ─────────────────────────────────────────────────

// RestrictField refuses a write that carries `field` unless allow(ctx) returns
// true. It is the write-side twin of response.RedactField: the same predicate
// shape, on the opposite step.
//
// Use it when a field is writable, but not by everyone — the case the static
// mfx tags cannot express. `readonly` means "no client ever writes this", so it
// needs no predicate; this is for "only a superuser may set
// subscription_expires_at, while the owner may write the rest of their own row".
// Without it, that split costs a separate endpoint per privileged field.
//
//	server.Pipeline.Validate.Register(
//	    validate.RestrictField("document_quota_bytes",
//	        func(ctx *maniflex.ServerContext) bool {
//	            return ctx.HasRole("superuser") || isBillingAdmin(ctx)
//	        }),
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
//
// A caller without permission gets 403 FIELD_FORBIDDEN naming the field, rather
// than having it silently dropped the way `readonly` drops one. The distinction
// is deliberate: a client sending a `readonly` field is confused about the
// schema, but a client sending this one is making a privilege error — the write
// it asked for is a write someone can do. Answering 200 to a write that did not
// happen, with the old value echoed back, is indistinguishable from success.
//
// Only a field actually present in the request body is gated, so a PATCH that
// does not mention it is unaffected. `field` is the JSON name.
//
// Scope it with maniflex.ForModel: registered without one it applies to every
// model, and a model that has no such field can never trigger it. RestrictField
// warns once per model when the field is not on that model, since a typo would
// otherwise leave the real field ungated in silence.
func RestrictField(field string, allow func(ctx *maniflex.ServerContext) bool) maniflex.MiddlewareFunc {
	var warned sync.Map // model name → struct{}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		warnFieldMissing(ctx, &warned, field)

		if _, present := ctx.ParsedBody.Map()[field]; !present {
			return next()
		}
		if allow != nil && allow(ctx) {
			return next()
		}
		ctx.Response = &maniflex.APIResponse{
			StatusCode: http.StatusForbidden,
			Error: &maniflex.APIError{
				Code:    "FIELD_FORBIDDEN",
				Message: fmt.Sprintf("you are not permitted to write field %q", field),
				Details: []map[string]string{{
					"field":   field,
					"message": "insufficient permissions to write this field",
				}},
			},
		}
		return nil
	}
}

// FieldRole refuses a write that carries `field` unless the caller holds one of
// `roles` (OR-semantics, matching maniflex.HasRole). It is RestrictField with
// the predicate every caller would otherwise write by hand:
//
//	server.Pipeline.Validate.Register(
//	    validate.FieldRole("subscription_expires_at", "superuser"),
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
//
// With no roles passed it rejects every write of the field — the same defensive
// choice auth.RequireRole and workflow.RequireRole make, so an accidentally
// empty role list fails closed rather than gating nothing. An unauthenticated
// caller holds no roles and is refused.
//
// See RestrictField for the semantics, and for gates roles cannot express
// (ownership, tenant, plan tier).
func FieldRole(field string, roles ...string) maniflex.MiddlewareFunc {
	return RestrictField(field, func(ctx *maniflex.ServerContext) bool {
		for _, r := range roles {
			if ctx.HasRole(r) {
				return true
			}
		}
		return false
	})
}

// warnFieldMissing logs once per model when the gated field is not one of the
// model's fields. Such a gate is inert: it watches for a body key that this
// model's write path would ignore anyway. That is harmless when the middleware
// is registered across models, and a silent hole when it is a typo — the real
// field keeps its name and nothing gates it. Warning is the only way to tell
// those apart, since the registration cannot see the model.
func warnFieldMissing(ctx *maniflex.ServerContext, warned *sync.Map, field string) {
	if ctx.Model == nil || ctx.Model.FieldByJSONName(field) != nil {
		return
	}
	if _, dup := warned.LoadOrStore(ctx.Model.Name, struct{}{}); dup {
		return
	}
	ctx.Logger().Warn(
		"[maniflex] validate: field gate names a field this model does not have — it can never fire; check the spelling, or scope the registration with maniflex.ForModel",
		"model", ctx.Model.Name, "field", field)
}
