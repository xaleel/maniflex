package maniflex

import (
	"context"
	"encoding/json"
	"net/http"
)

// ── Context ───────────────────────────────────────────────────────────────────

// OpenAPIContext is the shared object threaded through the OpenAPI pipeline.
// It is separate from ServerContext because the OpenAPI endpoint is not tied to
// any specific model or CRUD operation.
type OpenAPIContext struct {
	Request *http.Request
	Writer  http.ResponseWriter
	Ctx     context.Context

	// Auth is populated by the Auth step (same pattern as ServerContext).
	Auth *AuthInfo

	// Spec is set by the Generate step. Middleware registered After Generate
	// can read and mutate it to add custom paths, extensions, or security schemes.
	Spec *OpenAPISpec

	// Response overrides the default JSON serialisation when set by middleware.
	// If nil after the Response step runs, the default handler writes Spec as JSON.
	Response *APIResponse

	values map[string]any
}

// Set stores a custom value for downstream middleware.
func (c *OpenAPIContext) Set(key string, val any) {
	if c.values == nil {
		c.values = make(map[string]any)
	}
	c.values[key] = val
}

// Get retrieves a value stored by an earlier middleware.
func (c *OpenAPIContext) Get(key string) (any, bool) {
	if c.values == nil {
		return nil, false
	}
	v, ok := c.values[key]
	return v, ok
}

// Abort sets an error response, short-circuiting the pipeline.
func (c *OpenAPIContext) Abort(statusCode int, code, message string) {
	c.Response = &APIResponse{
		StatusCode: statusCode,
		Error:      &APIError{Code: code, Message: message},
	}
}

// ── Middleware type ───────────────────────────────────────────────────────────

// OpenAPIMiddlewareFunc is the middleware signature for the OpenAPI pipeline.
// Call next() to proceed; return without calling next() to short-circuit.
type OpenAPIMiddlewareFunc func(ctx *OpenAPIContext, next func() error) error

// ── Step registry ─────────────────────────────────────────────────────────────

type oasEntry struct {
	fn  OpenAPIMiddlewareFunc
	pos Position
}

// OpenAPIStepRegistry holds middlewares for one OpenAPI pipeline step.
// Unlike StepRegistry it has no ForModel/ForOperation filtering because the
// OpenAPI endpoint is a single, model-agnostic route.
type OpenAPIStepRegistry struct {
	name      string
	defaultFn OpenAPIMiddlewareFunc
	entries   []oasEntry
}

func newOASStepRegistry(name string, fn OpenAPIMiddlewareFunc) *OpenAPIStepRegistry {
	return &OpenAPIStepRegistry{name: name, defaultFn: fn}
}

// Register adds a middleware to this step.
//
// pos controls where relative to the default handler the middleware runs:
//
//	Before  (default) — runs before the default handler
//	After             — runs after the default handler
//	Replace           — replaces the default handler entirely
//
// Examples:
//
//	// Require admin role to read the spec
//	pipeline.OpenAPI.Auth.Register(requireAdmin)
//
//	// Add a custom extension after the spec is generated
//	pipeline.OpenAPI.Generate.Register(addMyExtension, maniflex.After)
//
//	// Serve the spec from a static file instead of generating it
//	pipeline.OpenAPI.Generate.Register(serveFromFile, maniflex.Replace)
func (s *OpenAPIStepRegistry) Register(fn OpenAPIMiddlewareFunc, pos ...Position) {
	p := Before
	if len(pos) > 0 {
		p = pos[0]
	}
	s.entries = append(s.entries, oasEntry{fn: fn, pos: p})
}

// build composes the chain for this step:
// before-middlewares → (default or replace) → after-middlewares
func (s *OpenAPIStepRegistry) build() OpenAPIMiddlewareFunc {
	var before, after []OpenAPIMiddlewareFunc
	var replaceFn OpenAPIMiddlewareFunc

	for _, e := range s.entries {
		switch e.pos {
		case Before:
			before = append(before, e.fn)
		case After:
			after = append(after, e.fn)
		case Replace:
			replaceFn = e.fn
		}
	}

	core := s.defaultFn
	if replaceFn != nil {
		core = replaceFn
	}

	chain := append(append(before, core), after...)
	return buildOASChain(chain)
}

func buildOASChain(chain []OpenAPIMiddlewareFunc) OpenAPIMiddlewareFunc {
	return func(ctx *OpenAPIContext, outerNext func() error) error {
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

// ── Pipeline ──────────────────────────────────────────────────────────────────

// OpenAPIPipeline is the middleware injection surface for the spec endpoint.
//
// The three steps run in order:
//
//	Auth     → guard access (passthrough by default)
//	Generate → build *OpenAPISpec from the registry (default) or replace it
//	Response → serialise Spec to JSON (default)
//
// Usage:
//
//	// Require a Bearer token to see the spec
//	server.Pipeline.OpenAPI.Auth.Register(myAuthMiddleware)
//
//	// Inject a custom extension after generation
//	server.Pipeline.OpenAPI.Generate.Register(func(ctx *maniflex.OpenAPIContext, next func() error) error {
//	    if err := next(); err != nil { return err }
//	    ctx.Spec.Info.Description = "My custom API"
//	    ctx.Spec.Components.SecuritySchemes = map[string]maniflex.OASSecurityScheme{
//	        "bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
//	    }
//	    return nil
//	}, maniflex.After)
//
//	// Serve a hand-crafted spec file instead
//	server.Pipeline.OpenAPI.Generate.Register(serveStaticSpec, maniflex.Replace)
type OpenAPIPipeline struct {
	Auth     *OpenAPIStepRegistry
	Generate *OpenAPIStepRegistry
	Response *OpenAPIStepRegistry
}

// newOpenAPIPipeline wires the three step registries with their default handlers.
func newOpenAPIPipeline(steps *oasDefaultSteps) *OpenAPIPipeline {
	return &OpenAPIPipeline{
		Auth:     newOASStepRegistry("openapi.auth", steps.auth),
		Generate: newOASStepRegistry("openapi.generate", steps.generate),
		Response: newOASStepRegistry("openapi.response", steps.response),
	}
}

// execute runs the three-step pipeline for one OpenAPI request.
func (p *OpenAPIPipeline) execute(ctx *OpenAPIContext) error {
	if err := p.Auth.build()(ctx, func() error { return nil }); err != nil {
		return err
	}
	if err := p.Generate.build()(ctx, func() error { return nil }); err != nil {
		return err
	}
	if err := p.Response.build()(ctx, func() error { return nil }); err != nil {
		return err
	}
	return nil
}

// handler returns an http.HandlerFunc that drives the OpenAPI pipeline.
func (p *OpenAPIPipeline) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := &OpenAPIContext{
			Request: r,
			Writer:  w,
			Ctx:     r.Context(),
		}
		if err := p.execute(ctx); err != nil {
			http.Error(w,
				`{"error":{"code":"INTERNAL","message":"internal server error"}}`,
				http.StatusInternalServerError)
			return
		}
		if ctx.Response != nil {
			ctx.Response.Write(w)
			return
		}
		// Fallback: if Response step didn't run (unusual), emit ctx.Spec
		// directly via the standard envelope-less path.
		if ctx.Spec != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ctx.Spec)
		}
	}
}

// ── Default step handlers ─────────────────────────────────────────────────────

type oasDefaultSteps struct {
	reg          RegistryAccessor
	cfg          *Config
	actions      []ActionConfig      // appended by Server.Action()
	globalSearch *GlobalSearchConfig // set by Server.EnableGlobalSearch()
}

func newOASDefaultSteps(reg RegistryAccessor, cfg *Config) *oasDefaultSteps {
	return &oasDefaultSteps{reg: reg, cfg: cfg}
}

// auth is a passthrough by default.
func (s *oasDefaultSteps) auth(ctx *OpenAPIContext, next func() error) error {
	return next()
}

// generate builds the OpenAPISpec from the registry and stores it on ctx.Spec.
func (s *oasDefaultSteps) generate(ctx *OpenAPIContext, next func() error) error {
	ctx.Spec = GenerateSpec(s.reg, s.cfg, s.actions, s.globalSearch)
	return next()
}

// response serialises ctx.Spec as JSON and sets ctx.Response.
// If ctx.Response is already set (by middleware or Abort), it honours that instead.
//
// The spec body is marshalled with json.MarshalIndent for human-readable
// output and parked on ctx.Response.Data as a json.RawMessage; AsIs=true so
// APIResponse.Write emits it verbatim (Encode honours RawMessage's
// MarshalJSON) without re-wrapping in a {"data": ...} envelope.
func (s *oasDefaultSteps) response(ctx *OpenAPIContext, next func() error) error {
	if ctx.Response != nil {
		return next()
	}
	if ctx.Spec == nil {
		ctx.Abort(http.StatusInternalServerError, "NO_SPEC",
			"Generate step did not produce a spec")
		return next()
	}

	b, err := json.MarshalIndent(ctx.Spec, "", "  ")
	if err != nil {
		ctx.Abort(http.StatusInternalServerError, "MARSHAL_ERROR", err.Error())
		return next()
	}

	// Set CORS header before the handler calls Write (must precede WriteHeader).
	// Swagger UI loads the spec cross-origin, so this stays permissive.
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")

	ctx.Response = &APIResponse{
		StatusCode: http.StatusOK,
		AsIs:       true,
		Data:       json.RawMessage(b),
	}

	return next()
}
