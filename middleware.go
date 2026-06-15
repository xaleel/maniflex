package maniflex

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// MiddlewareFunc is the signature every pipeline middleware must satisfy.
// Call next() to proceed to the next handler in the chain.
// Return without calling next() to short-circuit the pipeline (e.g. on auth failure).
type MiddlewareFunc func(ctx *ServerContext, next func() error) error

// Position controls where in a step's execution chain a middleware is inserted.
type Position int

const (
	// Before inserts the middleware before the default step handler (default).
	Before Position = iota
	// After inserts the middleware after the default step handler.
	After
	// Replace swaps out the default step handler entirely with this middleware.
	Replace
)

// MiddlewareConfig holds the filter criteria for a registered middleware.
// An empty Models slice means "all models"; an empty Operations slice means
// "all operations".
type MiddlewareConfig struct {
	Models     []string    // restrict to these model names (empty = all)
	Operations []Operation // restrict to these operations (empty = all)
	Position   Position
	Name       string
}

// MiddlewareOption is a functional option applied to a MiddlewareConfig.
type MiddlewareOption func(*MiddlewareConfig)

// ForModel restricts the middleware to the named model(s).
//
//	pipeline.Auth.Register(requireLogin, maniflex.ForModel("User", "Post"))
func ForModel(names ...string) MiddlewareOption {
	return func(c *MiddlewareConfig) { c.Models = append(c.Models, names...) }
}

// ForOperation restricts the middleware to the given operation(s).
//
//	pipeline.DB.Register(auditLog, maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
func ForOperation(ops ...Operation) MiddlewareOption {
	return func(c *MiddlewareConfig) { c.Operations = append(c.Operations, ops...) }
}

// AtPosition sets where the middleware sits relative to the default handler.
//
//	pipeline.Response.Register(addHeaders, maniflex.AtPosition(maniflex.After))
func AtPosition(p Position) MiddlewareOption {
	return func(c *MiddlewareConfig) { c.Position = p }
}

// WithName sets name for middleware for debug purposes
//
//	pipeline.Auth.Register(rateLimit, maniflex.WithName("rate-limiter"))
func WithName(name string) MiddlewareOption {
	return func(c *MiddlewareConfig) { c.Name = name }
}

// ── Internal ──────────────────────────────────────────────────────────────────

// namedFn pairs a MiddlewareFunc with the step and middleware names used in
// trace log records.
type namedFn struct {
	step string // display name of the pipeline step, e.g. "Auth", "DB"
	name string // name of this middleware, e.g. "JWTMiddleware", "default"
	fn   MiddlewareFunc
}

type registeredMiddleware struct {
	fn  MiddlewareFunc
	cfg MiddlewareConfig
}

func (m *registeredMiddleware) appliesTo(model string, op Operation) bool {
	modelMatch := len(m.cfg.Models) == 0
	for _, n := range m.cfg.Models {
		if n == model {
			modelMatch = true
			break
		}
	}
	opMatch := len(m.cfg.Operations) == 0
	for _, o := range m.cfg.Operations {
		if o == op {
			opMatch = true
			break
		}
	}
	return modelMatch && opMatch
}

// StepRegistry holds all middlewares registered for one pipeline step.
// Obtain one from the Pipeline struct and call Register() on it.
type StepRegistry struct {
	name          string         // internal lowercase key, e.g. "auth", "db"
	displayName   string         // display name for trace logs, e.g. "Auth", "DB"
	defaultFn     MiddlewareFunc // built-in handler for this step
	defaultFnName string         // trace name for the built-in handler, e.g. "default"
	middlewares   []registeredMiddleware
}

func newStepRegistry(name, defaultFnName string, defaultFn MiddlewareFunc) *StepRegistry {
	display := strings.ToUpper(name[:1]) + name[1:]
	if name == "db" {
		display = "DB"
	}
	return &StepRegistry{
		name:          name,
		displayName:   display,
		defaultFn:     defaultFn,
		defaultFnName: defaultFnName,
	}
}

// Register adds a middleware to this pipeline step.
//
// Without options the middleware applies to every model and operation.
// Combine options to narrow the scope:
//
//	// Run before the DB step for all POST /users requests
//	pipeline.DB.Register(hashPassword,
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	)
//
//	// Run after the Response step for every request
//	pipeline.Response.Register(addCorpHeaders, maniflex.AtPosition(maniflex.After))
//
//	// Replace the default Service step for Post creates
//	pipeline.Service.Register(publishPost,
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpCreate),
//	    maniflex.AtPosition(maniflex.Replace),
//	)
func (s *StepRegistry) Register(fn MiddlewareFunc, opts ...MiddlewareOption) {
	cfg := MiddlewareConfig{Position: Before, Name: "[unnamed]"}
	for _, o := range opts {
		o(&cfg)
	}
	s.middlewares = append(s.middlewares, registeredMiddleware{fn: fn, cfg: cfg})
}

// build returns the composed MiddlewareFunc for the given model+operation pair.
// Order: before-middlewares → (default or replace) → after-middlewares.
// Named middleware metadata is preserved for trace logging.
func (s *StepRegistry) build(model string, op Operation) MiddlewareFunc {
	var before, after []namedFn
	var skipped []namedFn
	coreFn := s.defaultFn
	coreName := s.defaultFnName

	for _, m := range s.middlewares {
		nfn := namedFn{step: s.displayName, name: m.cfg.Name, fn: m.fn}
		if !m.appliesTo(model, op) {
			skipped = append(skipped, nfn)
			continue
		}
		switch m.cfg.Position {
		case Before:
			before = append(before, nfn)
		case After:
			after = append(after, nfn)
		case Replace:
			coreFn = m.fn // last Replace wins
			coreName = m.cfg.Name
		}
	}

	core := namedFn{step: s.displayName, name: coreName, fn: coreFn}
	chain := append(append(before, core), after...)
	return buildNamedChain(chain, skipped)
}

// buildNamedChain composes a slice of namedFns into a single MiddlewareFunc.
// When ctx.trace is non-nil it wraps each middleware with enter/exit logging,
// optional timing, and abort-site detection. Skipped middlewares are logged
// once (before the chain runs) when ctx.trace.Skips is true.
func buildNamedChain(chain []namedFn, skipped []namedFn) MiddlewareFunc {
	return func(ctx *ServerContext, outerNext func() error) error {
		tr := ctx.trace
		// Log skipped middlewares once per request when Skips tracing is on.
		if tr != nil && tr.Skips {
			for _, nfn := range skipped {
				ctx.Logger().Debug("middleware skipped",
					slog.String("step", nfn.step),
					slog.String("middleware", nfn.name),
				)
			}
		}

		var run func(i int) error
		run = func(i int) error {
			if i >= len(chain) {
				return outerNext()
			}
			nfn := chain[i]
			if tr == nil || !tr.Steps {
				return nfn.fn(ctx, func() error { return run(i + 1) })
			}

			// ── traced path ───────────────────────────────────────────────
			log := ctx.Logger()
			log.Debug("middleware enter",
				slog.String("step", nfn.step),
				slog.String("middleware", nfn.name),
			)

			start := time.Now()
			err := nfn.fn(ctx, func() error { return run(i + 1) })
			dur := time.Since(start)

			attrs := []slog.Attr{
				slog.String("step", nfn.step),
				slog.String("middleware", nfn.name),
			}
			if tr.Timings {
				attrs = append(attrs, slog.String("duration", FormatDuration(dur)))
			}
			// Report abort if Abort() was called and not yet reported by a child
			// wrapper (abortSite is cleared after first reporting to prevent
			// outer wrappers from re-logging the same abort).
			if tr.Aborts && ctx.abortSite != "" {
				attrs = append(attrs,
					slog.Int("aborted_status", ctx.Response.StatusCode),
					slog.String("aborted_code", ctx.Response.Error.Code),
					slog.String("abort_site", ctx.abortSite),
				)
				ctx.abortSite = "" // consumed — outer wrappers will not re-log
			}
			log.LogAttrs(context.Background(), slog.LevelDebug, "middleware exit", attrs...)
			return err
		}
		return run(0)
	}
}

// buildChain composes a flat slice of MiddlewareFuncs into one left-to-right
// chain. Used by pipeline.execute() to chain the six step-level MiddlewareFuncs;
// per-middleware tracing is handled inside each step's buildNamedChain.
func buildChain(chain []MiddlewareFunc) MiddlewareFunc {
	return func(ctx *ServerContext, outerNext func() error) error {
		var run func(i int) error
		run = func(i int) error {
			if i >= len(chain) {
				return outerNext()
			}
			return chain[i](ctx, func() error { return run(i + 1) })
		}
		return run(0)
	}
}
