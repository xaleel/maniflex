package db

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/xaleel/maniflex"
)

// Custom server.Action handlers run a trimmed pipeline (Auth → action middleware
// → handler → Response) that skips the Deserialize/Validate/Service/DB steps. The
// standard RateLimit and AuditLog register on the DB step, so they never fire for
// actions. The *Action variants below run in the action's own Middleware list
// (after the global Auth step, so ctx.Auth is populated) and cover those routes.

// rateLimitCaller resolves the per-caller key: the configured KeyFunc, else the
// authenticated user id, else the remote IP.
func rateLimitCaller(cfg RateLimitConfig) func(*maniflex.ServerContext) string {
	if cfg.KeyFunc != nil {
		return cfg.KeyFunc
	}
	return func(ctx *maniflex.ServerContext) string {
		if ctx.Auth != nil && ctx.Auth.UserID != "" {
			return ctx.Auth.UserID
		}
		return ctx.Request.RemoteAddr
	}
}

// RateLimitAction is RateLimit for custom actions. It keys on the caller plus the
// request method+path (an action has no model/operation to key on), and returns
// 429 with a Retry-After header when the window is exceeded. Attach it in the
// action's Middleware list:
//
//	server.Action(maniflex.ActionConfig{
//	    Method: "POST", Path: "/reports/{id}/evidence",
//	    Middleware: []maniflex.MiddlewareFunc{
//	        db.RateLimitAction(db.RateLimitConfig{RequestsPerMinute: 10}),
//	    },
//	    Handler: uploadEvidence,
//	})
func RateLimitAction(cfg RateLimitConfig) maniflex.MiddlewareFunc {
	if cfg.RequestsPerMinute <= 0 {
		cfg.RequestsPerMinute = 60
	}
	if cfg.ErrorMessage == "" {
		cfg.ErrorMessage = "rate limit exceeded — try again later"
	}
	caller := rateLimitCaller(cfg)
	windowDur := cfg.Window
	if windowDur <= 0 {
		windowDur = time.Minute
	}
	retryAfter := fmt.Sprintf("%d", int(windowDur.Seconds()))
	limiter := &rateLimiter{windows: make(map[string]*window)}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		key := fmt.Sprintf("%s:%s:%s", caller(ctx), ctx.Request.Method, ctx.Request.URL.Path)
		over, err := rateLimitCheck(ctx, cfg, limiter, key, windowDur)
		if err != nil {
			return err
		}
		if over {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusTooManyRequests,
				Error:      &maniflex.APIError{Code: "RATE_LIMITED", Message: cfg.ErrorMessage},
			}
			ctx.Writer.Header().Set("Retry-After", retryAfter)
			return nil
		}
		return next()
	}
}

// AuditLogAction writes an audit record after a successful custom action. Unlike
// the DB-step AuditLog it sources everything from the action context: actor and
// tenant from ctx.Auth, the resource id from the "id" URL param (when present),
// the result from ctx.Response.Data, and Operation = OpAction. Change diffing
// (WithChanges) does not apply to actions, so it is ignored here.
//
// Writes are fire-and-forget: a sink error never fails the request.
//
//	server.Action(maniflex.ActionConfig{
//	    Method: "DELETE", Path: "/me",
//	    Middleware: []maniflex.MiddlewareFunc{ db.AuditLogAction(mySink) },
//	    Handler:    deactivate,
//	})
func AuditLogAction(sink AuditSink) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}
		rec := auditBuildActionRecord(ctx)
		ctx.GoBackground(func(bgCtx context.Context) {
			writeCtx, cancel := context.WithTimeout(bgCtx, 5*time.Second)
			defer cancel()
			if err := sink.Write(writeCtx, rec); err != nil {
				ctx.Logger().Warn("auditlog: action sink write failed",
					"path", ctx.Request.URL.Path,
					"error", err.Error())
			}
		})
		return nil
	}
}

func auditBuildActionRecord(ctx *maniflex.ServerContext) AuditRecord {
	actor, tenantID := "", ""
	if ctx.Auth != nil {
		actor = ctx.Auth.UserID
		tenantID = ctx.Auth.TenantID
	}
	var result any
	if ctx.Response != nil {
		result = ctx.Response.Data
	}
	return AuditRecord{
		Timestamp:   time.Now().UTC(),
		Model:       ctx.Request.Method + " " + ctx.Request.URL.Path,
		Operation:   maniflex.OpAction,
		ResourceID:  ctx.URLParam("id"),
		Actor:       actor,
		TenantID:    tenantID,
		RequestID:   ctx.RequestID,
		TraceID:     ctx.TraceID,
		ServiceName: ctx.ServiceName(),
		Result:      result,
	}
}
