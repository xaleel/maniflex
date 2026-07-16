package maniflex

import (
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
)

// Pipeline is the public middleware injection surface.
// It holds one StepRegistry per pipeline step, in execution order:
//
//	Auth → Deserialize → Validate → Service → DB → Response
//
// It also holds an OpenAPIPipeline for the schema endpoint:
//
//	OpenAPI.Auth → OpenAPI.Generate → OpenAPI.Response
//
// Inject middleware into any step via its Register() method:
//
//	server.Pipeline.Auth.Register(myJWTMiddleware)
//	server.Pipeline.Service.Register(myBusinessLogic, maniflex.ForModel("Order"))
//	server.Pipeline.DB.Register(myAuditLogger, maniflex.AtPosition(maniflex.After))
//	server.Pipeline.OpenAPI.Auth.Register(requireAdmin)
//	server.Pipeline.OpenAPI.Generate.Register(addExtensions, maniflex.After)
type Pipeline struct {
	// Auth — identity and access control (passthrough by default).
	Auth *StepRegistry

	// Deserialize — parse JSON body into ParsedBody; parse URL query params into Query.
	Deserialize *StepRegistry

	// Validate — enforce mfx struct-tag rules (required, readonly, immutable, enum, min/max).
	Validate *StepRegistry

	// Service — user-injectable business logic (noop by default).
	Service *StepRegistry

	// DB — dispatch to the configured DBAdapter.
	DB *StepRegistry

	// Response — build the JSON response envelope from ctx.DBResult.
	Response *StepRegistry

	// OpenAPI — three-step pipeline for the GET /openapi.json endpoint.
	// Steps: Auth → Generate → Response
	OpenAPI *OpenAPIPipeline

	// frozen mirrors the step registries: set once, when the router is built.
	frozen atomic.Bool

	// chains memoizes the whole six-step chain per (model, operation), so a request
	// costs one lookup instead of six compositions plus a buildChain (PERF-2). Only
	// written after frozen, so entries can never go stale.
	chains sync.Map // chainKey → MiddlewareFunc
}

// freeze closes the registration window on every step and switches the pipeline to
// its cached chains. The router calls it once it has finished registering its own
// middleware (the file-cleanup hooks), after which the composed chains are fixed.
func (p *Pipeline) freeze() {
	for _, s := range p.steps() {
		s.freeze()
	}
	p.frozen.Store(true)
}

// newPipeline wires all step registries with their built-in default handlers.
func newPipeline(s *defaultSteps, oas *oasDefaultSteps) *Pipeline {
	return &Pipeline{
		Auth:        newStepRegistry("auth", "default", s.auth),
		Deserialize: newStepRegistry("deserialize", "default", s.deserialize),
		Validate:    newStepRegistry("validate", "default", s.validate),
		Service:     newStepRegistry("service", "default", s.service),
		DB:          newStepRegistry("db", "default", s.db),
		Response:    newStepRegistry("response", "default", s.response),
		OpenAPI:     newOpenAPIPipeline(oas),
	}
}

// executeAction runs the trimmed pipeline for custom action endpoints:
//
//	Auth → [per-action middleware...] → handler → [response middleware...] → Response
//
// Deserialize, Validate, Service, and DB steps are intentionally skipped.
// Middleware registered on those steps with ForOperation(OpAction) is inert.
func (p *Pipeline) executeAction(ctx *ServerContext, cfg ActionConfig) error {
	model := ctx.Model.Name // synthetic name, never empty
	op := ctx.Operation     // OpAction

	// Wrap the handler as a MiddlewareFunc so it participates in the chain
	// and calls next() to pass control to the Response step.
	handlerFn := func(ctx *ServerContext, next func() error) error {
		if err := cfg.Handler(ctx); err != nil {
			return err
		}
		return next()
	}

	// Flatten: Auth → middleware... → handler → responseMiddleware... → Response
	steps := make([]MiddlewareFunc, 0, len(cfg.Middleware)+len(cfg.ResponseMiddleware)+3)
	steps = append(steps, p.Auth.build(model, op))
	steps = append(steps, cfg.Middleware...)
	steps = append(steps, handlerFn)
	steps = append(steps, cfg.ResponseMiddleware...)
	steps = append(steps, p.Response.build(model, op))

	return buildChain(steps)(ctx, func() error { return nil })
}

// executeSearch runs the trimmed pipeline for the built-in cross-model search
// endpoint (GET /search):
//
//	Auth → handler → Response
//
// Deserialize, Validate, Service, and DB are intentionally skipped — the handler
// performs the search via ctx.Search. Mirrors executeAction; middleware
// registered on the skipped steps with ForOperation(OpSearch) is inert (and
// warned about at startup by warnIneffectiveMiddleware).
func (p *Pipeline) executeSearch(ctx *ServerContext, handler func(*ServerContext) error) error {
	model := ctx.Model.Name // synthetic "__search", never empty
	op := ctx.Operation     // OpSearch

	handlerFn := func(ctx *ServerContext, next func() error) error {
		if err := handler(ctx); err != nil {
			return err
		}
		return next()
	}

	steps := []MiddlewareFunc{
		p.Auth.build(model, op),
		handlerFn,
		p.Response.build(model, op),
	}
	return buildChain(steps)(ctx, func() error { return nil })
}

// execute runs the complete six-step pipeline for ctx.
// If any step sets ctx.Response without calling next(), the remaining
// steps are skipped and the response is written directly.
func (p *Pipeline) execute(ctx *ServerContext) error {
	// Wrap the composed chain so the last step's next() is a no-op.
	return p.chainFor(ctx.Model.Name, ctx.Operation)(ctx, func() error { return nil })
}

// chainFor returns the six-step chain for one model+operation pair, composing it
// on first use and serving it from cache thereafter. Before the router is built it
// always composes: the registration window is still open, so the answer can change.
func (p *Pipeline) chainFor(model string, op Operation) MiddlewareFunc {
	k := chainKey{model: model, op: op}
	if p.frozen.Load() {
		if fn, ok := p.chains.Load(k); ok {
			return fn.(MiddlewareFunc)
		}
	}

	fn := buildChain([]MiddlewareFunc{
		p.Auth.build(model, op),
		p.Deserialize.build(model, op),
		p.Validate.build(model, op),
		p.Service.build(model, op),
		p.DB.build(model, op),
		p.Response.build(model, op),
	})

	if p.frozen.Load() {
		p.chains.Store(k, fn)
	}
	return fn
}

// steps returns the six core step registries in execution order. Used by
// warnIneffectiveMiddleware to scan every registered middleware. The OpenAPI
// sub-pipeline is intentionally excluded — it has its own steps and operations.
func (p *Pipeline) steps() []*StepRegistry {
	return []*StepRegistry{p.Auth, p.Deserialize, p.Validate, p.Service, p.DB, p.Response}
}

// stepsSkippedByOp records, per operation, the step registry names whose pipeline
// does not execute for that operation. It is the single source of truth shared by
// the trimmed pipelines (executeAction, executeSearch) and the startup
// ineffective-registration scan below — a future op that trims the pipeline adds
// one entry here.
var stepsSkippedByOp = map[Operation][]string{
	OpAction: {"deserialize", "validate", "service", "db"},
	OpSearch: {"deserialize", "validate", "service", "db"},
}

// opSkipsStep reports whether the pipeline for op skips the step named stepName.
func opSkipsStep(op Operation, stepName string) bool {
	return slices.Contains(stepsSkippedByOp[op], stepName)
}

// warnIneffectiveMiddleware logs a warning for every middleware registered on a
// step its operation filter can never reach — e.g. ForOperation(OpSearch) on the
// Service step, which only runs for full-pipeline operations. It warns only when
// EVERY operation in the filter skips the step (so a mixed filter like
// ForOperation(OpCreate, OpAction) on Service, still effective for OpCreate, is
// not flagged) and never for an unfiltered middleware (which applies to all ops).
//
// This mirrors the warn-and-drop convention in flattenArgs; a future Config.Strict
// mode (roadmap §10.1) will promote these warnings to panics.
func warnIneffectiveMiddleware(p *Pipeline, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, sr := range p.steps() {
		for _, m := range sr.middlewares {
			if middlewareNeverRunsOnStep(m.cfg.Operations, sr.name) {
				logger.Warn("[maniflex] middleware registered for an operation whose pipeline skips this step — it will never run",
					slog.String("step", sr.displayName),
					slog.String("middleware", m.cfg.Name),
					slog.Any("operations", m.cfg.Operations))
			}
		}
	}
}

// middlewareNeverRunsOnStep reports whether a middleware filtered to ops can
// never execute on the step named stepName — true only when ops is non-empty and
// every listed operation skips that step. An empty ops (all operations) is always
// effective, so it returns false.
func middlewareNeverRunsOnStep(ops []Operation, stepName string) bool {
	if len(ops) == 0 {
		return false
	}
	for _, op := range ops {
		if !opSkipsStep(op, stepName) {
			return false
		}
	}
	return true
}
