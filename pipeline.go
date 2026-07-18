package maniflex

import (
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
// a startup error — see collectIneffectiveMiddleware).
func (p *Pipeline) executeSearch(ctx *ServerContext, handler func(*ServerContext) error) error {
	return p.executeTrimmed(ctx, handler)
}

// executeTrimmed runs Auth → handler → Response for an operation that has no
// body to deserialize, no rules to validate and no row to read or write. The
// skipped steps must be declared in stepsSkippedByOp, or the startup scan will
// not know to warn about middleware registered on them.
//
// Auth is never among the skipped: an operation that reaches data — even to name
// it, as minting an upload URL does — is one the app's auth must gate.
func (p *Pipeline) executeTrimmed(ctx *ServerContext, handler func(*ServerContext) error) error {
	model := ctx.Model.Name
	op := ctx.Operation

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
// collectIneffectiveMiddleware to scan every registered middleware. The OpenAPI
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
	OpAction:        {"deserialize", "validate", "service", "db"},
	OpSearch:        {"deserialize", "validate", "service", "db"},
	OpPresignUpload: {"deserialize", "validate", "service", "db"},
}

// opSkipsStep reports whether the pipeline for op skips the step named stepName.
func opSkipsStep(op Operation, stepName string) bool {
	return slices.Contains(stepsSkippedByOp[op], stepName)
}

// collectIneffectiveMiddleware reports every middleware registered on a step its
// operation filter can never reach — e.g. ForOperation(OpSearch) on the Service
// step, which only runs for full-pipeline operations. It reports only when EVERY
// operation in the filter skips the step (so a mixed filter like
// ForOperation(OpCreate, OpAction) on Service, still effective for OpCreate, is
// not flagged) and never for an unfiltered middleware (which applies to all ops).
//
// This is a startup error rather than a warning (10.1) because the middleware is
// registered and frozen into the chain, and then never runs. There is no reading
// of it under which the author got what they wrote, and when the middleware is
// an authorisation check the result is a silent hole.
func (p *Pipeline) collectIneffectiveMiddleware(issues *issueList) {
	for _, sr := range p.steps() {
		for _, m := range sr.middlewares {
			if !middlewareNeverRunsOnStep(m.cfg.Operations, sr.name) {
				continue
			}
			name := m.cfg.Name
			if name == "" {
				name = "<unnamed>"
			}
			issues.add("middleware",
				"middleware %q is registered on the %s step for operation(s) %v, all of which "+
					"skip that step — it will never run; register it on a step those "+
					"operations run, or widen ForOperation",
				name, sr.displayName, m.cfg.Operations)
		}
	}
}

// collectFieldRequirementIssues reports every RequiresField declaration that no
// model can satisfy (roadmap 10.2).
//
// A field-gating middleware whose field name is misspelt is a silent hole: the
// gate watches for a body key nothing sends, the real field keeps its name, and
// nothing gates it. The middleware itself cannot detect this — at request time
// "this model has no such field" is equally consistent with a gate deliberately
// registered across models where only some carry it. The model scope is known
// only at the registration, which is why the declaration lives there.
//
// That scope is also what makes the check exact rather than a guess.
func (p *Pipeline) collectFieldRequirementIssues(reg *Registry, issues *issueList) {
	for _, sr := range p.steps() {
		for i := range sr.middlewares {
			m := &sr.middlewares[i]
			for _, field := range m.cfg.RequiredFields {
				checkFieldRequirement(reg, sr.displayName, m, field, issues)
			}
		}
	}
}

// checkFieldRequirement validates one declared field against the registration's
// model scope.
func checkFieldRequirement(reg *Registry, step string, m *registeredMiddleware,
	field string, issues *issueList,
) {
	name := m.cfg.Name
	if name == "" || name == "[unnamed]" {
		name = "<unnamed>"
	}

	// Scoped with ForModel: the gate was aimed at these models specifically, so
	// every one of them must carry the field.
	if len(m.cfg.Models) > 0 {
		for _, modelName := range m.cfg.Models {
			model, ok := reg.Get(modelName)
			if !ok {
				// ForModel naming an unregistered model is its own problem and
				// not this check's to report; saying the model lacks a field
				// when the model does not exist would only mislead.
				continue
			}
			if model.FieldByJSONName(field) == nil {
				issues.add("middleware",
					"middleware %q on the %s step declares RequiresField(%q), but model %q has "+
						"no field of that name — check the spelling, or drop the model from "+
						"ForModel", name, step, field, modelName)
			}
		}
		return
	}

	// Unscoped: it applies to every model, so it is enough that one of them can
	// trigger it. If none can, the gate cannot be doing anything anywhere.
	for _, model := range reg.All() {
		if model.FieldByJSONName(field) != nil {
			return
		}
	}
	issues.add("middleware",
		"middleware %q on the %s step declares RequiresField(%q), but no registered model has a "+
			"field of that name, so it can never fire — check the spelling", name, step, field)
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
