package maniflex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// parseTraceparent validates a W3C traceparent header and returns it
// (lowercased) when well-formed, or "" otherwise. The header has the shape
// "<version>-<trace-id>-<parent-id>-<flags>" where version is 2 hex chars,
// trace-id is 32 hex chars, parent-id is 16 hex chars, and flags is 2 hex chars.
//
// The W3C spec mandates lowercase hex, but several real-world tracers (older
// OpenTelemetry JS, some Datadog libs) emit mixed case. We accept either and
// lowercase the value before forwarding so downstream services see canonical
// form regardless of what the client sent.
func parseTraceparent(v string) string {
	if len(v) != 55 {
		return ""
	}
	// Cheap structural check: dashes at fixed positions.
	if v[2] != '-' || v[35] != '-' || v[52] != '-' {
		return ""
	}
	for i, c := range v {
		if i == 2 || i == 35 || i == 52 {
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return ""
		}
	}
	return strings.ToLower(v)
}

// handlers owns the shared dependencies for every generated HTTP handler.
type handlers struct {
	pipeline *Pipeline
	steps    *defaultSteps // needed to wire ctx.adapter
	cfg      *Config       // needed for QueryTimeout

	// globalSearch is non-nil when Server.EnableGlobalSearch was called; it
	// drives mounting and configuration of the built-in /search endpoint.
	globalSearch *GlobalSearchConfig
}

func newHandlers(p *Pipeline, s *defaultSteps, cfg *Config) *handlers {
	return &handlers{pipeline: p, steps: s, cfg: cfg}
}

// buildContext constructs a ServerContext for a single request and returns a
// cleanup function the caller MUST defer. The cleanup releases:
//   - The per-request QueryTimeout context (if configured).
//   - Any multipart temp files chi/net-http parked under r.MultipartForm.
//   - Any open file readers parked on ctx.Files by the multipart parser.
//
// Centralising the construction here prevents the dispatch / dispatchWith /
// Action paths from drifting apart (roadmap §11B.3) — previously the Action
// path was missing the ctx.Files cleanup and leaked open readers when an
// action consumed a multipart upload.
func (h *handlers) buildContext(w http.ResponseWriter, r *http.Request, meta *ModelMeta, op Operation) (*ServerContext, func()) {
	reqID := chiMiddleware.GetReqID(r.Context())

	// Start from the HTTP request context so cancellation from client disconnect
	// propagates naturally into the pipeline.
	baseCtx := r.Context()

	// Attach a per-request deadline when QueryTimeout is configured.
	// The cancel func is always invoked to release the deadline timer and
	// prevent goroutine leaks — safe even when the pipeline panics because
	// the panic recoverer (mounted above dispatch in the chi router) catches
	// it and the deferred cleanup still runs during unwinding.
	var cancel context.CancelFunc
	if h.cfg.QueryTimeout > 0 {
		baseCtx, cancel = context.WithTimeout(baseCtx, h.cfg.QueryTimeout)
	}

	ctx := &ServerContext{
		Request:   r,
		Writer:    w,
		Ctx:       baseCtx,
		Model:     meta,
		Operation: op,
		RequestID: reqID,
		TraceID:   parseTraceparent(r.Header.Get("traceparent")),
		// Expose the adapter so middleware can call ctx.BeginTx without
		// holding a direct reference to the adapter.
		adapter:     h.steps.adapter,
		reg:         h.steps.reg,
		keyProvider: h.steps.keyProvider,
		logger:      h.cfg.logger(),
		serviceName: h.cfg.ServiceName,
		trace:       h.cfg.traceConfig(),
		bg:          h.steps.bg,
	}
	if id := chi.URLParam(r, "id"); id != "" {
		ctx.ResourceID = id
	}

	// Echo the request ID back in the response header so clients can correlate
	// their request to log lines. Set before pipeline.execute() so the header
	// is present even when a middleware short-circuits with an error response.
	if reqID != "" {
		w.Header().Set("X-Request-Id", reqID)
	}
	if h.cfg.ServiceName != "" {
		w.Header().Set("X-Service-Name", h.cfg.ServiceName)
	}

	cleanup := func() {
		if cancel != nil {
			cancel()
		}
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		for _, f := range ctx.Files {
			if f.Reader != nil {
				f.Reader.Close()
			}
		}
	}
	return ctx, cleanup
}

// StatusClientClosedRequest is the status recorded when the caller hangs up
// before the response is written — nginx's non-standard 499. Nothing reaches the
// client (the connection is already gone), but this is what access logs, metrics
// and any middleware reading the response status will see, so a disconnect is
// told apart from a genuine server-side timeout (504) rather than being counted
// as one.
const StatusClientClosedRequest = 499

// clientGone reports whether the caller hung up. net/http cancels the request's
// own context when the connection drops; a QueryTimeout firing leaves that
// context alone and cancels the derived ctx.Ctx instead. Without this the two
// are indistinguishable — the pipeline reports both as a context error.
func clientGone(ctx *ServerContext) bool {
	if ctx == nil || ctx.Request == nil {
		return false
	}
	return errors.Is(ctx.Request.Context().Err(), context.Canceled)
}

// clientGoneResponse is the empty 499 the pipeline hands back once the caller
// has disconnected: a status for the logs, no body for nobody.
func clientGoneResponse() *APIResponse {
	return &APIResponse{StatusCode: StatusClientClosedRequest, AsIs: true}
}

// writePipelineError translates a pipeline error to an HTTP response and logs
// the failure. A disconnected client becomes 499 (logged at DEBUG, not as a
// server error); a ctx deadline or cancellation becomes 504 TIMEOUT; everything
// else is logged and becomes 500 INTERNAL. The extra slog fields differ between
// model dispatch and Action dispatch so they're passed in by the caller.
func (h *handlers) writePipelineError(w http.ResponseWriter, ctx *ServerContext, err error, errFields ...any) {
	fields := append([]any{slog.String("error", err.Error())}, errFields...)

	// The caller left. Whatever we write goes to a closed connection, and calling
	// it a timeout blames the server for the client's decision — and cites a
	// QueryTimeout that may not even be configured.
	if clientGone(ctx) {
		ctx.Logger().Debug("client disconnected before the response was written", fields...)
		w.WriteHeader(StatusClientClosedRequest)
		return
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		http.Error(w,
			`{"error":{"code":"TIMEOUT","message":"request exceeded the configured query timeout"}}`,
			http.StatusGatewayTimeout)
		return
	}
	ctx.Logger().Error("unhandled pipeline error", fields...)
	http.Error(w,
		`{"error":{"code":"INTERNAL","message":"internal server error"}}`,
		http.StatusInternalServerError)
}

// dispatch builds a ServerContext, runs the pipeline, and writes the response.
func (h *handlers) dispatch(w http.ResponseWriter, r *http.Request, meta *ModelMeta, op Operation) {
	h.dispatchWith(w, r, meta, op, nil)
}

// dispatchWith is dispatch with a hook to populate context fields specific to
// the operation before pipeline.execute is called.
func (h *handlers) dispatchWith(w http.ResponseWriter, r *http.Request, meta *ModelMeta, op Operation, hook func(*ServerContext)) {
	ctx, cleanup := h.buildContext(w, r, meta, op)
	defer cleanup()

	if hook != nil {
		hook(ctx)
	}

	if err := h.pipeline.execute(ctx); err != nil {
		ctx.cleanupOrphanedFiles()
		h.writePipelineError(w, ctx, err,
			slog.String("model", meta.Name),
			slog.String("op", string(op)))
		return
	}
	writeResponse(w, ctx)
}

// writeResponse writes ctx.Response to the client. When the request ended with a
// non-2xx status, it first rolls back any files stored during the request so a
// failed create/update does not leak blobs in storage (3B.2b). On a 2xx (or
// unset) status the write is known to have landed, so any files a successful
// update replaced are deleted then (BUG-1). The two are mutually exclusive: a
// failed write never deletes the old blob it still references.
func writeResponse(w http.ResponseWriter, ctx *ServerContext) {
	if ctx.Response == nil {
		return
	}
	if sc := ctx.Response.StatusCode; sc != 0 && (sc < 200 || sc >= 300) {
		ctx.cleanupOrphanedFiles()
	} else {
		ctx.deleteReplacedFiles()
	}
	ctx.Response.Write(w)
}

// List returns a handler for GET /resource
func (h *handlers) List(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpList)
	}
}

// Read returns a handler for GET /resource/{id}
func (h *handlers) Read(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpRead)
	}
}

// Create returns a handler for POST /resource
func (h *handlers) Create(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpCreate)
	}
}

// Update returns a handler for PATCH /resource/{id}
func (h *handlers) Update(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpUpdate)
	}
}

// Delete returns a handler for DELETE /resource/{id}
func (h *handlers) Delete(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpDelete)
	}
}

// SingletonRead returns a handler for GET /resource on a ModelConfig.Singleton
// model. There is no {id} in the URL, so it pins ResourceID to SingletonID; the
// DB step provisions that row on first access (see ensureSingletonRow).
func (h *handlers) SingletonRead(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatchWith(w, r, meta, OpRead, func(ctx *ServerContext) {
			ctx.ResourceID = SingletonID
		})
	}
}

// SingletonUpdate returns a handler for PATCH /resource on a
// ModelConfig.Singleton model — the update counterpart of SingletonRead.
func (h *handlers) SingletonUpdate(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatchWith(w, r, meta, OpUpdate, func(ctx *ServerContext) {
			ctx.ResourceID = SingletonID
		})
	}
}

// Export returns a handler for GET /resource/export. Mounted only when the
// model opts in via ModelConfig.ExportEnabled. The Response step detects
// OpExport and streams CSV/XLSX directly.
func (h *handlers) Export(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatch(w, r, meta, OpExport)
	}
}

// Aggregate returns a handler for GET /resource/aggregate. Mounted only when the
// model opts in via ModelConfig.AggregateEnabled. It dispatches as OpList — so
// auth and row-isolation middleware registered for the list operation apply
// unchanged — and sets ctx.aggregate, which routes the Deserialize, DB, and
// Response steps to parse the JSON body, run ctx.Aggregate, and emit group rows
// instead of performing a normal list read.
func (h *handlers) Aggregate(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatchWith(w, r, meta, OpList, func(ctx *ServerContext) {
			ctx.aggregate = true
		})
	}
}

// GlobalSearch returns the handler for the built-in cross-model search endpoint
// (GET /search, mounted only when Server.EnableGlobalSearch was called). It runs
// the trimmed Auth → handler → Response pipeline (executeSearch) on a synthetic
// "__search" model, so global Auth middleware and ForOperation(OpSearch)
// middleware apply. The handler parses the query, resolves the model set, runs
// ctx.Search, and emits the merged hits under the standard {"data": ...} envelope.
func (h *handlers) GlobalSearch(cfg GlobalSearchConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cleanup := h.buildContext(w, r, searchSyntheticModel(), OpSearch)
		defer cleanup()

		handler := func(ctx *ServerContext) error { return h.runGlobalSearch(ctx, cfg) }

		if err := h.pipeline.executeSearch(ctx, handler); err != nil {
			ctx.cleanupOrphanedFiles()
			h.writePipelineError(w, ctx, err, slog.String("op", string(OpSearch)))
			return
		}
		writeResponse(w, ctx)
	}
}

// runGlobalSearch is the search handler body: parse ?q=/?limit=/?models=, run
// ctx.Search over the GlobalSearchable model set, and set ctx.Response. Returns a
// non-nil error only on an unexpected ctx.Search failure (→ 500); all client
// input problems call ctx.Abort and return nil.
func (h *handlers) runGlobalSearch(ctx *ServerContext, cfg GlobalSearchConfig) error {
	q := strings.TrimSpace(ctx.QueryParam("q"))
	if q == "" {
		ctx.Abort(http.StatusBadRequest, "INVALID_QUERY", "search query (?q=) must not be empty")
		return nil
	}

	limit, ok := parseSearchLimit(ctx, cfg)
	if !ok {
		return nil // ctx.Abort already called
	}
	models, ok := parseSearchModels(ctx)
	if !ok {
		return nil // ctx.Abort already called
	}

	results, err := ctx.Search(SearchOptions{Query: q, Models: models, Limit: limit})
	if err != nil {
		return err
	}
	if results == nil {
		results = []SearchResult{}
	}
	ctx.Response = &APIResponse{StatusCode: http.StatusOK, Data: results}
	return nil
}

// parseSearchLimit reads ?limit=, defaulting to cfg.DefaultLimit and clamping to
// cfg.MaxLimit. Returns (limit, false) after calling ctx.Abort on a bad value.
func parseSearchLimit(ctx *ServerContext, cfg GlobalSearchConfig) (int, bool) {
	limit := cfg.DefaultLimit
	if raw := ctx.QueryParam("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			ctx.Abort(http.StatusBadRequest, "INVALID_QUERY", "?limit= must be a non-negative integer")
			return 0, false
		}
		if n > 0 {
			limit = n
		}
	}
	if cfg.MaxLimit > 0 && limit > cfg.MaxLimit {
		limit = cfg.MaxLimit
	}
	return limit, true
}

// parseSearchModels reads the optional ?models= CSV, validating every name
// against the GlobalSearchable set. An empty/absent param returns (nil, true) so
// ctx.Search falls back to all GlobalSearchable models. Returns (nil, false)
// after calling ctx.Abort on an unknown/unexposed model.
func parseSearchModels(ctx *ServerContext) ([]string, bool) {
	raw := ctx.QueryParam("models")
	if raw == "" {
		return nil, true
	}
	allowed := globalSearchableModels(ctx.reg)
	var models []string
	for name := range strings.SplitSeq(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !allowed[name] {
			ctx.Abort(http.StatusBadRequest, "INVALID_QUERY",
				fmt.Sprintf("model %q is not available for global search", name))
			return nil, false
		}
		models = append(models, name)
	}
	return models, true
}

// globalSearchableModels returns the set of model names opted into the built-in
// /search endpoint (ModelConfig.GlobalSearchable). Used to validate ?models=.
func globalSearchableModels(reg RegistryAccessor) map[string]bool {
	out := make(map[string]bool)
	if reg == nil {
		return out
	}
	for _, m := range reg.All() {
		if m.Config.GlobalSearchable && len(m.SearchFields) > 0 {
			out[m.Name] = true
		}
	}
	return out
}

// Attachment returns a handler for GET /resource/{id}/{file_field}.
// Reuses the read pipeline so registered Auth, soft-delete, and tenancy
// middleware apply identically to attachment downloads — the Response step
// detects OpReadAttachment + ctx.AttachmentField and streams the file
// instead of writing a JSON envelope.
func (h *handlers) Attachment(meta *ModelMeta, field FieldMeta) http.HandlerFunc {
	fieldCopy := field
	return func(w http.ResponseWriter, r *http.Request) {
		h.dispatchWith(w, r, meta, OpReadAttachment, func(ctx *ServerContext) {
			ctx.AttachmentField = &fieldCopy
		})
	}
}

// Head returns a handler for HEAD /resource[/{id}?].
//
// HEAD is GET with the body suppressed (RFC 9110 §9.3.2), so it dispatches as
// the read operation the same URL would serve for GET — OpRead on an item,
// OpList on a collection — and net/http drops the body while keeping the status
// and headers. Dispatching it as the underlying read (rather than as its own
// operation) is what keeps HEAD honest: it 404s for a missing record instead of
// always answering 200, and every read-scoped middleware — auth, tenancy,
// soft-delete — applies exactly as it does to GET, so a HEAD probe can't become
// an existence oracle that walks around ForOperation(OpRead) auth (BUG-9).
func (h *handlers) Head(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if meta.Config.Singleton {
			h.dispatchWith(w, r, meta, OpRead, func(ctx *ServerContext) {
				ctx.ResourceID = SingletonID
			})
			return
		}
		op := OpList
		if chi.URLParam(r, "id") != "" {
			op = OpRead
		}
		h.dispatch(w, r, meta, op)
	}
}

// Options returns a handler for OPTIONS /resource[/{id}?]. It advertises the
// methods the router actually mounts for that path (RFC 9110 §10.2.1) and
// answers 204 with no body.
func (h *handlers) Options(meta *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowedMethods(meta, chi.URLParam(r, "id") != ""))
		h.dispatch(w, r, meta, OpOptions)
	}
}

// allowedMethods lists the methods mounted for a model's collection or item
// path, mirroring mountModel.
func allowedMethods(meta *ModelMeta, isItem bool) string {
	switch {
	case meta.Config.Singleton:
		return "GET, HEAD, PATCH, OPTIONS"
	case isItem:
		return "GET, HEAD, PATCH, DELETE, OPTIONS"
	default:
		return "GET, HEAD, POST, OPTIONS"
	}
}

// Action returns an http.HandlerFunc for a custom action endpoint.
// syntheticModel is the sentinel ModelMeta created per ActionConfig.
func (h *handlers) Action(cfg ActionConfig, syntheticModel *ModelMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cleanup := h.buildContext(w, r, syntheticModel, OpAction)
		defer cleanup()

		if err := h.pipeline.executeAction(ctx, cfg); err != nil {
			ctx.cleanupOrphanedFiles()
			h.writePipelineError(w, ctx, err,
				slog.String("method", cfg.Method),
				slog.String("path", cfg.Path))
			return
		}
		writeResponse(w, ctx)
	}
}
