package maniflex

// ActionHandlerFunc is the signature for custom action handler functions.
// The handler must either set ctx.Response or call ctx.Abort before returning nil.
// If neither is done, the Response step defaults to 200 OK with no body.
type ActionHandlerFunc func(ctx *ServerContext) error

// ActionConfig configures one custom action endpoint.
type ActionConfig struct {
	// Routing — path must use chi v5 {param} syntax, not :param.
	Method string // HTTP method: "GET", "POST", "PATCH", "PUT", "DELETE"
	Path   string // e.g. "/appointments/{id}/cancel"

	// Behaviour
	Handler    ActionHandlerFunc // required
	Middleware []MiddlewareFunc  // run between global Auth and Handler; nil is fine

	// ResponseMiddleware runs after Handler but before the Response step writes
	// the response — the per-action counterpart to Pipeline.Response. Use it to
	// post-process ctx.Response (headers, envelope shaping) for one action
	// without registering against the synthetic model name. nil is fine.
	ResponseMiddleware []MiddlewareFunc

	// OpenAPI metadata (all optional)
	Tags       []string // defaults to ["Actions"] when empty
	Summary    string
	Deprecated bool

	// OpenAPI schema hints (all optional)
	//
	// RequestBody describes the expected request body for POST/PUT/PATCH actions.
	// Use maniflex.JSONRequestBody(schema) to build one from an *OASSchema.
	//
	// Responses maps HTTP status codes to response body schemas.
	// A nil schema means no response body (suitable for 204 No Content).
	// When set, these replace the default generated responses entirely.
	//
	// Example:
	//   Responses: map[int]*maniflex.OASSchema{
	//     201: {Type: "object", Properties: map[string]*maniflex.OASSchema{"id": {Type: "string"}}},
	//     404: nil,
	//   },
	RequestBody *OASRequestBody
	Responses   map[int]*OASSchema

	// OpenAPI carries richer, optional OpenAPI metadata for this action,
	// most notably request/response schemas inferred from Go structs. It
	// complements the inline RequestBody / Responses fields above, which take
	// precedence when both are set. See ActionOpenAPI.
	OpenAPI ActionOpenAPI
}

// ActionOpenAPI holds optional OpenAPI documentation for a custom action that
// goes beyond the inline ActionConfig.RequestBody / Responses fields. Every
// field is optional and the zero value adds nothing to the spec.
//
// The headline feature is schema inference: set RequestSchema / ResponseSchema
// to a Go struct value (or pointer) annotated with the same json + mfx tags as
// models, and the OpenAPI generator reflects it into an application/json schema
// — no hand-written *OASSchema required.
//
//	server.Action(maniflex.ActionConfig{
//	    Method: "POST",
//	    Path:   "/appointments/{id}/reschedule",
//	    OpenAPI: maniflex.ActionOpenAPI{
//	        RequestSchema:  RescheduleRequest{},  // {new_time string `mfx:"required"`}
//	        ResponseSchema: Appointment{},
//	        ResponseStatus: 200,
//	        QueryParams: []maniflex.OASParameter{{
//	            Name: "notify", In: "query",
//	            Schema: &maniflex.OASSchema{Type: "boolean"},
//	        }},
//	        Security: []map[string][]string{{"bearerAuth": {}}},
//	    },
//	    Handler: rescheduleHandler,
//	})
type ActionOpenAPI struct {
	// RequestSchema is a Go struct value, pointer, or reflect.Type whose
	// exported fields (read via json + mfx tags) become an application/json
	// request body schema. Ignored when ActionConfig.RequestBody is set.
	RequestSchema any

	// ResponseSchema is a Go struct value, pointer, or reflect.Type reflected
	// into the success-response body schema. Ignored when ActionConfig.Responses
	// is set.
	ResponseSchema any

	// ResponseStatus is the HTTP status the ResponseSchema documents.
	// Defaults to 200 when zero.
	ResponseStatus int

	// Description is a long-form operation description (Markdown). Summary on
	// ActionConfig remains the short one-liner.
	Description string

	// QueryParams declares query-string parameters in addition to the path
	// parameters extracted automatically from the route.
	QueryParams []OASParameter

	// Security lists security requirement objects for the operation, e.g.
	// []map[string][]string{{"bearerAuth": {}}}. The referenced schemes must
	// be registered separately (see middleware/openapi.AddSecurityScheme).
	Security []map[string][]string
}

// JSONRequestBody is a convenience constructor that wraps an *OASSchema in a
// required application/json request body — the common case for action endpoints.
func JSONRequestBody(schema *OASSchema) *OASRequestBody {
	return &OASRequestBody{
		Required: true,
		Content:  map[string]OASMediaType{"application/json": {Schema: schema}},
	}
}

// actionSyntheticModel returns a minimal *ModelMeta used as a sentinel so
// pipeline steps do not panic on ctx.Model.Name. The name is unique per
// method+path so model-scoped middleware cannot accidentally apply.
func actionSyntheticModel(method, path string) *ModelMeta {
	return &ModelMeta{Name: "__action_" + method + "_" + path}
}
