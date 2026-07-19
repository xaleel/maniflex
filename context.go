package maniflex

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Operation identifies which CRUD action a request performs.
type Operation string

const (
	OpCreate  Operation = "create"
	OpRead    Operation = "read"
	OpUpdate  Operation = "update"
	OpDelete  Operation = "delete"
	OpList    Operation = "list"
	OpOptions Operation = "options"

	// OpHead is retained for compatibility but is never set on a request. HEAD is
	// GET with the body suppressed, so a HEAD request dispatches as the read it
	// mirrors — OpRead for an item, OpList for a collection — and every middleware
	// scoped to those operations applies to it unchanged. Scope HEAD-aware
	// middleware with ForOperation(OpRead)/ForOperation(OpList), not with this.
	OpHead Operation = "head"

	// OpReadAttachment identifies a request for GET /:model/:id/:file_field —
	// the per-model attachment route. Runs Auth → Deserialize → Validate → DB
	// (FindByID) like a regular Read, then the Response step streams the
	// referenced file instead of writing a JSON envelope.
	//
	// Middleware filtered with ForOperation(OpRead) DOES match attachment
	// requests: an attachment is a read of one record, so whatever decides who
	// may read it decides this too (audit MS-8). The implication is one-way —
	// ForOperation(OpReadAttachment) stays attachment-only.
	OpReadAttachment Operation = "read_attachment"

	// OpReadHistory identifies a request for GET /:model/:id/history — the
	// per-record version history of a ModelConfig.Versioned model (audit MS-4).
	//
	// It dispatches against the **parent** model, not the synthesized history
	// model, and that is the whole point. The history table has none of the
	// parent's columns — no tenant_id, no owner_id, nothing a scope could filter
	// on — so it cannot be secured on its own terms. Running the parent's read
	// pipeline first means the caller must be allowed to read the record before
	// its history is fetched, and every auth, tenancy and force-filter middleware
	// already registered ForModel(parent) governs it unchanged.
	//
	// Middleware filtered with ForOperation(OpRead) DOES match history requests
	// (audit MS-8), and that is load-bearing rather than a convenience: the gate
	// above reads the request's *forced* filters, so a tenancy middleware scoped
	// to OpRead that never ran would leave the gate with nothing to scope by and
	// hand every tenant's history to every caller. The implication is one-way —
	// ForOperation(OpReadHistory) stays history-only.
	OpReadHistory Operation = "read_history"

	// OpAction identifies a request handled by a custom action registered via
	// server.Action(). Deserialize, Validate, Service, and DB steps are skipped;
	// Auth and Response run normally.
	// Note: registering middleware on Pipeline.Deserialize/Validate/Service/DB
	// with ForOperation(OpAction) is valid syntax but has no effect — those steps
	// do not execute for actions. The server logs a warning at startup for such a
	// registration (see warnIneffectiveMiddleware in pipeline.go).
	OpAction Operation = "action"

	// OpSearch identifies a request to the built-in cross-model search endpoint
	// (GET /search, enabled via Server.EnableGlobalSearch). Like OpAction it runs
	// only Auth → handler → Response; Deserialize, Validate, Service, and DB are
	// skipped, so the handler performs the search via ctx.Search.
	//
	// The synthetic model name is "__search", so ForModel never matches it; gate
	// middleware on this endpoint with ForOperation(OpSearch) or register it
	// globally on Pipeline.Auth. Registering on Deserialize/Validate/Service/DB
	// with ForOperation(OpSearch) has no effect and is warned about at startup
	// (see warnIneffectiveMiddleware in pipeline.go).
	OpSearch Operation = "search"

	// OpExport identifies a request for GET /:model/export — the auto-generated
	// CSV/XLSX export endpoint. The DB step reads with the same filter/sort as
	// OpList (no pagination; capped at ModelConfig.MaxExportRows). The Response
	// step streams CSV (default) or XLSX (?format=xlsx) directly instead of
	// emitting a JSON envelope.
	//
	// Middleware filtered with ForOperation(OpList) DOES match exports: an
	// export is a list in another format, so list-scoped auth and tenancy
	// cover it (audit MS-8). The implication is one-way —
	// ForOperation(OpExport) stays export-only.
	OpExport Operation = "export"

	// Minting a presigned upload for an mfx:"file,upload:presigned" field —
	// POST /{model}/{field}/upload-url. It runs a trimmed pipeline of
	// Auth → handler → Response: there is no body to validate, no record to read,
	// and no row to write, so Deserialize, Validate, Service and DB are skipped.
	//
	// Auth runs, which is the point: minting a URL is granting the right to write
	// an object, so it must be gated by whatever gates the model. Middleware
	// filtered with ForOperation(OpCreate) does NOT match it — the mint is not the
	// create, and can precede one by minutes.
	OpPresignUpload Operation = "presign_upload"
)

// AuthIdentityType classifies the principal behind an authenticated request.
type AuthIdentityType string

const (
	// IdentityAnonymous is the zero value — the request is unauthenticated or
	// the identity type was not set by the auth middleware.
	IdentityAnonymous AuthIdentityType = ""

	// IdentityHuman represents an interactive end-user (employee, patient, admin).
	IdentityHuman AuthIdentityType = "human"

	// IdentityServiceAccount represents a machine caller (background job,
	// service-to-service, API integration).
	IdentityServiceAccount AuthIdentityType = "service_account"
)

// AuthInfo carries identity information set by the Auth pipeline step.
// Populate it inside your auth middleware via ctx.Auth = &maniflex.AuthInfo{...}
type AuthInfo struct {
	// Core identity — present in every auth scheme.
	UserID string
	Roles  []string
	Claims map[string]any // raw token claims or equivalent; always non-nil when Auth is set

	// Multi-tenancy — the partition key for the authenticated principal.
	// Set by JWTAuth when JWTOptions.TenantClaim is configured, or directly
	// by any auth middleware (subdomain tenancy, X-Tenant-ID header, etc.).
	TenantID string

	// IdentityType classifies the caller. Zero value (IdentityAnonymous) means
	// the type was not determined; treat it as a human for backward compatibility.
	IdentityType AuthIdentityType

	// Scopes holds the OAuth2 scopes granted to the token.
	// Populated by JWTAuth from the configured ScopesClaim (default "scope").
	Scopes []string

	// SessionID is the JWT "jti" claim or session token ID. Empty for API-key
	// callers. Useful for read audit trails that need to record which session
	// accessed a record.
	SessionID string

	// AuthMethod describes how the caller authenticated.
	// JWTAuth sets this to "jwt"; APIKeyAuth sets it to "api_key".
	// Conventional values: "jwt", "api_key", "session", "oauth2".
	AuthMethod string
}

// ServerContext is the single shared object threaded through every pipeline step
// for one HTTP request. Steps read from it, write to it, and call next() to
// proceed to the following step.
type ServerContext struct {
	// HTTP primitives
	Request *http.Request
	Writer  http.ResponseWriter
	Ctx     context.Context

	// Routing context — set by the handler before pipeline.execute()
	Model      *ModelMeta
	Operation  Operation
	ResourceID string // primary key from the URL, present for read/update/delete

	// AttachmentField is set when Operation == OpReadAttachment. It points at
	// the file field whose contents the request is asking to stream. The
	// Response step consults it to dereference the storage key from
	// DBResult and serve the file. nil for all other operations.
	AttachmentField *FieldMeta

	// RequestID is the value of the X-Request-Id header set by chi's RequestID
	// middleware. It is extracted once in handler.dispatch and stored here so
	// every middleware can include it in structured log records without importing
	// chi or reading the HTTP header directly.
	//
	// The same value is echoed back in the X-Request-Id response header so
	// clients can correlate requests to log lines.
	RequestID string

	// TraceID is the value of the W3C traceparent header on the incoming
	// request, or empty when the header is absent or malformed. The full
	// header value is preserved verbatim so it can be forwarded to downstream
	// services (e.g. service.Webhook) without re-encoding.
	TraceID string

	// Step outputs — each step populates these in order
	RawBody []byte // raw request body bytes (set by Deserialize)
	// ParsedBody is the deserialized JSON body, JSON-keyed (set by Deserialize).
	// It is read-only: read via ctx.Field or ParsedBody's methods (Get/Has/Len/
	// Keys/Map), and mutate ONLY via ctx.SetField / ctx.DeleteField, which keep
	// the typed Record in sync. It is a *RequestBody (not a bare map) precisely so
	// a raw ctx.ParsedBody["k"]=v — which bypassed that sync and could drop the
	// write — is a compile error. nil when the request carries no body.
	//
	// Prefer Record (the typed *T carrier) where possible.
	ParsedBody *RequestBody
	// Record holds the typed record carrier (*T for ctx.Model.GoType) for write
	// operations (set by Deserialize) and, increasingly, read results. The
	// pipeline reads/writes record fields through it instead of ParsedBody.
	Record   any
	Query    *QueryParams // parsed URL query params (set by Deserialize)
	DBResult any          // *T, *ListResult, or map[string]any (set by DB step)
	Response *APIResponse // final response envelope (set by Response step)

	// Files holds parsed file uploads from multipart/form-data requests.
	// Keyed by the form field name (which matches the JSON field name).
	// Populated by the Deserialize step when Content-Type is multipart/form-data.
	Files map[string]*UploadedFile

	// Locale is the explicit locale requested by the client, resolved by
	// LocaleResolver middleware from ?locale= or Accept-Language. Empty when
	// the client did not request a specific locale. Used as the first entry in
	// the locale resolution chain for LocaleString fields.
	Locale string

	// DefaultLocale is the app-configured fallback locale from LocaleOptions.Default.
	// Set by LocaleResolver; empty when the middleware is not registered (falls
	// back to "en" in the resolution chain).
	DefaultLocale string

	// SplitSuffix is the suffix used for the i18n companion field in split mode
	// (e.g. "name_i18n"). Set by LocaleResolver from LocaleOptions.SplitSuffix;
	// defaults to "_i18n" when the middleware is not registered.
	SplitSuffix string

	// DefaultLocaleMode is the app-wide fallback LocaleMode from
	// LocaleOptions.DefaultLocaleMode. Set by LocaleResolver; empty when the
	// middleware is not registered (falls back to LocaleModeSplit).
	DefaultLocaleMode LocaleMode

	// Auth — set by the Auth step (your middleware fills this)
	Auth *AuthInfo

	// Tx is an optional active transaction. When non-nil the default DB step
	// routes all Create / Update / Delete / FindByID / FindMany calls through
	// it instead of the bare adapter, so every DB operation in the request
	// participates in the same transaction.
	//
	// Set this field from a Service or DB middleware using BeginTx:
	//
	//	tx, err := ctx.BeginTx(ctx.Ctx, nil)
	//	if err != nil { ... }
	//	ctx.Tx = tx
	//	defer tx.Rollback() // no-op after Commit
	Tx Tx

	// adapter is the underlying DBAdapter, exposed only for BeginTx.
	// It is set by the pipeline executor from the defaultSteps adapter.
	// Per-request resolution: the active adapter is ctx.Model.ResolveAdapter(adapter)
	// so a per-model Adapter override takes precedence over the global.
	adapter DBAdapter

	// reg is the model registry, used by QueryModel. Set by the handler.
	reg RegistryAccessor

	// actionScope is the row-level scope in force for a custom Action, set by
	// db.TenancyAction / db.ForceFilterAction (see action_scope.go). nil — the
	// case for every generated CRUD request, whose scoping the DB step owns —
	// means every DB path behaves exactly as it always has.
	actionScope *ActionScope

	// inProcess marks a request Server.Execute raised rather than one a client
	// sent. Read it through InProcess(); it is unexported so that only Execute can
	// set it — a middleware, and therefore anything a client can reach, cannot
	// claim to be internal. See execute.go.
	inProcess bool

	// keyProvider encrypts/decrypts mfx:"encrypted" fields for the non-pipeline
	// access paths (typed maniflex.Create/Read and ctx.GetModel). The handler sets
	// it from Config.KeyProvider; background contexts set it via SetKeyProvider.
	keyProvider KeyProvider

	// logger is the framework-level logger seeded from Config.Logger.
	// It is set by the handler before the pipeline runs.
	logger *slog.Logger

	// serviceName is copied from Config.ServiceName by the handler so
	// ServerContext.Logger() can include it as a "service" attribute without
	// holding a reference to the full Config.
	serviceName string

	// trace is non-nil when any PipelineTrace flag is set.
	// Checked at call time inside buildNamedChain; nil means tracing is off.
	trace *PipelineTrace

	// cachedLogger memoises the per-request *slog.Logger produced by Logger()
	// so middleware on the hot path doesn't pay a fresh base.With(...) call
	// on every Info/Debug emission. The derived attributes (service,
	// request_id, trace_id) are immutable for the lifetime of the request,
	// so a single build is sufficient.
	//
	// loggerOnce guards the build. The context outlives its request goroutine:
	// GoBackground closures capture it and log from another goroutine while the
	// request is still in later steps, so the lazy write raced those reads
	// (PERF-3). Once keeps the build lazy — a request that never logs still pays
	// nothing — while making it exactly-once, so every goroutine sees the same
	// logger.
	loggerOnce   sync.Once
	cachedLogger *slog.Logger

	// queryValues memoises the parsed URL query, built at most once by
	// queryParams(). url.Values is rebuilt from the raw string on every
	// URL.Query() call, so reading three parameters parsed it three times
	// (PERF-4). Lazy, so a request that never reads one parses nothing; Once
	// because the context is shared with ctx.GoBackground goroutines.
	queryOnce   sync.Once
	queryValues url.Values

	// existingCols memoises the DB-column map of the record as it stands before
	// this request's write, read at most once per request by
	// defaultSteps.existingColumns. The file step asks for it once per file field
	// being written, and each ask re-read the same row and rebuilt the same map
	// (PERF-4). nil when there is no such row (create, missing id, read error) —
	// callers treat that as "no previous value".
	existingOnce sync.Once
	existingCols map[string]any

	// bg is the Server-owned background runner. Middleware uses
	// GoBackground to schedule fire-and-forget work (audit writes, cache
	// invalidations) that Shutdown waits on rather than abandoning. May be
	// nil when ServerContext is synthesised outside the framework (tests,
	// custom action wrappers); GoBackground falls back to a bare goroutine.
	bg *backgroundRunner

	// abortSite is the file:line of the most recent Abort() call.
	// Populated only when trace.Aborts is true; cleared after it is logged by
	// the innermost middleware wrapper so outer wrappers do not re-log it.
	abortSite string

	// Arbitrary cross-step storage for middleware communication
	values map[string]any

	// present and selectKeys stage the typed-models carrier metadata captured by
	// the Deserialize step (Phase 1). present is the set of top-level JSON keys
	// in the write body (PATCH presence semantics); selectKeys is the ?select=
	// projection set (JSON names). Phase 4 copies these into the typed record's
	// BaseModel carriers (mfxSetPresent / mfxSetSelect). Inert until then.
	present    map[string]struct{}
	selectKeys map[string]struct{}

	// aggregate marks a request as the auto-generated GET /:model/aggregate
	// endpoint (ModelConfig.AggregateEnabled). The handler dispatches it as
	// OpList so list auth/tenancy middleware apply, then this flag routes the
	// Deserialize/DB/Response steps to parse the JSON body, run ctx.Aggregate,
	// and emit the group rows instead of a normal list read. aggQuery holds the
	// AggregateQuery parsed and validated by the Deserialize step.
	aggregate bool
	aggQuery  *AggregateQuery

	// restore marks a request as the POST /:model/:id/restore endpoint
	// (Config.RestoreEnabled, soft-delete models only). Like aggregate, the
	// handler dispatches it as an existing operation — OpUpdate — so that every
	// middleware an app already registered for "who may modify this row" covers
	// un-deleting it too, without anyone having to discover a new operation
	// constant. This flag then routes the steps that would otherwise assume a
	// body and a visible row: Deserialize skips body parsing (a restore carries
	// none), the row-reading write guards are skipped because a soft-deleted row
	// is invisible to every read path, and the DB step calls Restore instead of
	// Update. Read it from middleware with IsRestore.
	restore bool

	// redactedFields names response fields this request must not disclose,
	// declared through RedactResponseField (audit MS-11).
	//
	// A Response-step middleware masks by mutating ctx.Response after next()
	// returns. An export has no ctx.Response — it streams bytes and leaves it
	// nil — so such a middleware silently masked nothing, and an app that hid
	// salary from non-admins served it in full at /employees/export. The
	// declaration is what bridges that: it is recorded *before* next(), so the
	// export path, which runs inside next(), can honour it while still writing
	// one row at a time.
	redactedFields []string

	// maxBody overrides the default JSON body size limit for this request; zero
	// means maxBodyBytes. Set through SetMaxBodySize (body.MaxBodySize).
	maxBody int64

	// storedFiles records objects written to FileStorage during this request's
	// Service step (3B.2b). If the request ultimately fails after a file was
	// stored — a pipeline error or a non-2xx final response, including a
	// Validate/Auth-after-Service middleware that aborts post-store — the handler
	// deletes these orphans so a failed create/update never leaks blobs.
	// Populated via TrackStoredFile; drained by cleanupOrphanedFiles.
	storedFiles []storedFile

	// replacedFiles records existing objects that a successful write will
	// supersede — an mfx:"auto_delete" file being replaced or cleared on update.
	// They are deleted only if the request ends 2xx, so a failed update never
	// orphans the row by deleting a blob it still references (BUG-1). Populated
	// via trackReplacedFile; drained by deleteReplacedFiles.
	replacedFiles []storedFile

	// streamedFileFields records the JSON names of mfx:"file,upload:stream" fields
	// already stored during Deserialize, so the Service step's processFileFields
	// leaves them alone instead of re-processing the key it wrote as a reference
	// (which would cost a redundant Stat). Populated via markStreamedFile.
	streamedFileFields map[string]struct{}
}

// storedFile pairs an object key with the storage backend it was written to, so
// orphan cleanup can delete it without assuming a single global FileStorage.
type storedFile struct {
	key     string
	storage FileStorage
}

// SetMaxBodySize overrides the JSON body size limit for this request (default
// 4 MB). Call it from a Deserialize-step middleware, before the body is read —
// body.MaxBodySize does exactly this. A non-positive value is ignored.
func (c *ServerContext) SetMaxBodySize(limit int64) {
	if limit > 0 {
		c.maxBody = limit
	}
}

// bodyLimit is the size ceiling for this request's JSON body.
func (c *ServerContext) bodyLimit() int64 {
	if c.maxBody > 0 {
		return c.maxBody
	}
	return maxBodyBytes
}

// readLimitedBody reads the whole request body, rejecting anything over the
// limit instead of silently truncating it. It reads one byte past the limit as a
// sentinel: a body of exactly the limit is accepted, anything larger is caught.
//
// Truncating instead of rejecting is worse than it sounds — the surplus is cut
// off and the remainder either fails to parse as a misleading INVALID_JSON, or,
// when the cut lands after a complete JSON object, parses fine and the request
// is accepted on a body the client never sent (BUG-4).
//
// On failure it aborts the request and returns a non-nil error.
func (c *ServerContext) readLimitedBody() ([]byte, error) {
	limit := c.bodyLimit()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		// body.MaxBodySize wraps the body in http.MaxBytesReader; its overflow is
		// a size rejection, not an I/O failure.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, c.abortBodyTooLarge(maxErr.Limit)
		}
		c.Abort(http.StatusBadRequest, "BODY_READ_ERROR", "failed to read request body")
		return nil, fmt.Errorf("body read: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, c.abortBodyTooLarge(limit)
	}
	return body, nil
}

func (c *ServerContext) abortBodyTooLarge(limit int64) error {
	c.Abort(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
		fmt.Sprintf("request body exceeds %s limit", formatByteSize(limit)))
	return fmt.Errorf("body exceeds %d bytes", limit)
}

// BindJSON decodes the request body as JSON into v. It enforces the same 4 MB
// body size limit as the Deserialize step and sets ctx.RawBody. An absent body
// is rejected with 400 EMPTY_BODY — use TryBindJSON for GET or optional-body
// actions. On error it calls ctx.Abort and returns a non-nil error so the
// caller can return nil immediately:
//
//	var req MyRequest
//	if err := ctx.BindJSON(&req); err != nil {
//	    return nil // ctx.Abort already called
//	}
func (c *ServerContext) BindJSON(v any) error {
	ok, err := c.TryBindJSON(v)
	if err != nil {
		return err
	}
	if !ok {
		c.Abort(http.StatusBadRequest, "EMPTY_BODY", "request body must not be empty")
		return fmt.Errorf("empty body")
	}
	return nil
}

// TryBindJSON behaves like BindJSON but treats an absent body as a non-error,
// so GET or optional-body actions can share the same size-limited, abort-on-
// failure parsing path. It returns (false, nil) when the body is empty (v left
// untouched, ctx not aborted), (false, err) after calling ctx.Abort on an I/O,
// size, or parse failure, and (true, nil) on success with v populated and
// ctx.RawBody set:
//
//	ok, err := ctx.TryBindJSON(&req)
//	if err != nil {
//	    return nil // ctx.Abort already called
//	}
//	if ok {
//	    // req populated from a present body
//	}
func (c *ServerContext) TryBindJSON(v any) (ok bool, err error) {
	body, err := c.readLimitedBody()
	if err != nil {
		return false, err // ctx.Abort already called
	}
	if len(body) == 0 {
		return false, nil
	}
	c.RawBody = body
	if err := json.Unmarshal(body, v); err != nil {
		c.Abort(http.StatusBadRequest, "INVALID_JSON", fmt.Sprintf("malformed JSON: %s", err.Error()))
		return false, err
	}
	return true, nil
}

// EnsureRawBody returns the raw request body, reading and buffering it if no
// earlier step already has. The Deserialize step sets ctx.RawBody for create and
// update requests, but the trimmed action/search/presign pipelines skip
// Deserialize entirely and Deserialize never reads a body for GET/DELETE — so a
// middleware that must see the raw bytes before the handler binds them (an
// idempotency body hash, a signature check) calls this instead of reading
// ctx.RawBody directly, which would otherwise be nil in those contexts.
//
// The body is read once, under the same size limit as Deserialize (respecting
// SetMaxBodySize), cached on ctx.RawBody, and the request body is restored so a
// later ctx.BindJSON in the handler still sees it. Subsequent calls return the
// cached bytes without re-reading. A request with no body yields an empty,
// non-nil slice. On a size-limit or read failure it calls ctx.Abort and returns a
// non-nil error, so the caller can return nil immediately.
func (c *ServerContext) EnsureRawBody() ([]byte, error) {
	if c.RawBody != nil {
		return c.RawBody, nil
	}
	if c.Request == nil || c.Request.Body == nil {
		c.RawBody = []byte{}
		return c.RawBody, nil
	}
	body, err := c.readLimitedBody()
	if err != nil {
		return nil, err // ctx.Abort already called
	}
	if body == nil {
		body = []byte{}
	}
	c.RawBody = body
	// readLimitedBody drained the stream; restore it so a later reader — an action
	// handler's ctx.BindJSON — still sees the same bytes.
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// URLParam returns the value of a named URL parameter from the current request.
// Uses chi's URLParam under the hood; action handlers use this instead of
// importing chi directly.
//
//	// Route: /patients/{patientId}/notes/{noteId}
//	patientID := ctx.URLParam("patientId")
//	noteID    := ctx.URLParam("noteId")
func (c *ServerContext) URLParam(name string) string {
	return chi.URLParam(c.Request, name)
}

// QueryParam returns the first value of a URL query parameter, or "" if absent.
// Convenience wrapper around ctx.Request.URL.Query().Get(name).
//
//	date := ctx.QueryParam("date")   // ?date=2026-05-04
//
// The query string is parsed on the first call and reused for the rest of the
// request — url.Values is rebuilt from the raw string by every Query() call, so
// asking for three parameters used to parse it three times (PERF-4). A request
// that never asks for one parses nothing. Safe to call from any goroutine.
func (c *ServerContext) QueryParam(name string) string {
	return c.queryParams().Get(name)
}

// queryParams returns the request's parsed query string, parsing it at most once.
// Nothing in the framework rewrites URL.RawQuery mid-request, so a single parse is
// as current as a fresh one.
func (c *ServerContext) queryParams() url.Values {
	c.queryOnce.Do(func() {
		if c.Request == nil || c.Request.URL == nil {
			// A synthesised context (NewBackground, a hand-built test ctx) has no
			// request to read; an empty set keeps QueryParam total.
			c.queryValues = url.Values{}
			return
		}
		c.queryValues = c.Request.URL.Query()
	})
	return c.queryValues
}

// BeginTx starts a transaction on the underlying adapter and returns the Tx
// handle. It is a convenience wrapper so middleware does not need to hold a
// reference to the adapter directly.
//
//	tx, err := ctx.BeginTx(ctx.Ctx, nil)
//	if err != nil {
//	    return fmt.Errorf("begin tx: %w", err)
//	}
//	ctx.Tx = tx
//	defer tx.Rollback()
//	// ... do work ...
//	return tx.Commit()
//
// requestAdapter returns the adapter that should serve the current request:
// the per-model override on ctx.Model when set, otherwise the global adapter
// the context was constructed with. Used by BeginTx, RawQuery, and RawExec.
func (c *ServerContext) requestAdapter() DBAdapter {
	if c.Model != nil {
		return c.Model.ResolveAdapter(c.adapter)
	}
	return c.adapter
}

// BeginTx opens a transaction on the request's adapter.
//
// When an ActionScope is in force the returned Tx is scoped by it: reads are
// filtered, a create is stamped, and an update or delete of a record outside the
// scope returns ErrNotFound. ctx.Tx is a public field, so anything downstream
// that picks the transaction up is scoped too. ctx.Unscoped().BeginTx returns an
// unscoped transaction for work that genuinely must reach across the scope.
func (c *ServerContext) BeginTx(ctx context.Context, opts *TxOptions) (Tx, error) {
	tx, err := c.beginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	// Outermost, so these overrides win over tracedTx's — which beginTx has
	// already applied when tracing is on, and which still sees the real calls.
	if c.actionScope != nil {
		tx = &scopedTx{Tx: tx, ctx: c, scope: c.actionScope}
	}
	return tx, nil
}

func (c *ServerContext) beginTx(ctx context.Context, opts *TxOptions) (Tx, error) {
	adapter := c.requestAdapter()
	if adapter == nil {
		return nil, ErrNoAdapter
	}
	tx, err := adapter.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if c.trace != nil && c.trace.Steps {
		c.Logger().Debug("transaction begin")
		tx = &tracedTx{Tx: tx, ctx: c}
	}
	return tx, nil
}

// RawQuery executes a parameterised statement that returns rows and returns each
// row as a column-name → value map. This is a SELECT, a CTE-SELECT, or a
// data-modifying statement with a RETURNING clause (e.g.
// UPDATE … RETURNING id — a "claim-and-fetch" pattern). When ctx.Tx is active
// the query participates in the transaction; otherwise a plain SELECT uses the
// read pool and a RETURNING write uses the write pool.
//
// If a transaction is active but its Tx cannot run raw SQL, the call fails with
// ErrRawNotSupportedInTx rather than quietly running outside the transaction.
//
// Placeholders are rebound to the adapter's dialect, so `?` works on both SQLite
// and Postgres ($N). Never interpolate values directly into the query string.
//
// It refuses while an ActionScope is in force: the statement is opaque to the
// framework, so the scope cannot be applied to it and running it anyway would
// return every tenant's rows under a guarantee that says otherwise. Use the
// scoped paths, or ctx.Unscoped().RawQuery to step outside deliberately.
func (c *ServerContext) RawQuery(query string, args ...any) ([]Row, error) {
	if err := c.guardRaw("RawQuery()"); err != nil {
		return nil, err
	}
	return c.rawQuery(query, args...)
}

func (c *ServerContext) rawQuery(query string, args ...any) ([]Row, error) {
	if c.Tx != nil {
		rt, ok := c.Tx.(rawableT)
		if !ok {
			return nil, ErrRawNotSupportedInTx
		}
		return rt.RawQueryContext(c.Ctx, strings.TrimSpace(query), args...)
	}
	adapter := c.requestAdapter()
	if adapter == nil {
		return nil, ErrNoAdapter
	}
	result := adapter.Raw(c.Ctx, strings.TrimSpace(query), args...)
	rows, err := result.Rows()
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, nil
	}
	defer rows.Close()
	return scanSQLRows(rows)
}

// RawExec executes a parameterised non-SELECT statement and returns the
// number of rows affected. When ctx.Tx is active the statement participates
// in it; otherwise it uses the adapter's write pool.
//
// If a transaction is active but its Tx cannot run raw SQL, the call fails with
// ErrRawNotSupportedInTx rather than quietly running outside the transaction —
// where the write would commit on its own and survive the rollback.
//
// It refuses while an ActionScope is in force, for the same reason RawQuery does
// — and a write is the case where it matters most. Use ctx.Unscoped().RawExec to
// step outside deliberately.
func (c *ServerContext) RawExec(query string, args ...any) (int64, error) {
	if err := c.guardRaw("RawExec()"); err != nil {
		return 0, err
	}
	return c.rawExec(query, args...)
}

func (c *ServerContext) rawExec(query string, args ...any) (int64, error) {
	if c.Tx != nil {
		rt, ok := c.Tx.(rawableT)
		if !ok {
			return 0, ErrRawNotSupportedInTx
		}
		return rt.RawExecContext(c.Ctx, strings.TrimSpace(query), args...)
	}
	adapter := c.requestAdapter()
	if adapter == nil {
		return 0, ErrNoAdapter
	}
	result := adapter.Raw(c.Ctx, strings.TrimSpace(query), args...)
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

// adapterDriverTyper is an optional interface that SQL adapter implementations
// may satisfy to expose their driver dialect. Used by pkg/ledger and any other
// package that needs to build driver-aware parameterised queries.
type adapterDriverTyper interface {
	DriverType() DriverType
}

// DriverType returns the SQL dialect used by the adapter that will serve this
// request. Useful for packages that build raw SQL and need to choose between
// ? (SQLite) and $N (Postgres) placeholders.
//
// If the underlying adapter does not implement the optional DriverTyper
// interface, SQLite is returned as a safe default.
func (c *ServerContext) DriverType() DriverType {
	a := c.requestAdapter()
	if a != nil {
		if dt, ok := a.(adapterDriverTyper); ok {
			return dt.DriverType()
		}
	}
	return SQLite
}

// LockForUpdate acquires a pessimistic row-level lock on the named model's
// record identified by id and returns the current row data.
//
// For Postgres the underlying query appends FOR UPDATE; the lock is held until
// the enclosing transaction commits or rolls back. For SQLite the lock is the
// transaction's own write lock, taken at BEGIN — the sqlite adapter opens its
// write connections with _txlock=immediate, so a second read-then-write
// transaction waits at BEGIN instead of reading the row you are about to change.
//
// LockForUpdate requires an active transaction on ctx.Tx. Call ctx.BeginTx
// or register maniflex.WithTransaction before invoking it.
//
//	server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
//	    stockID, _ := ctx.Field("stock_id")
//	    stock, err := ctx.LockForUpdate("StockBalance", stockID.(string))
//	    if err != nil { return err }
//	    if stock["quantity"].(int64) < 1 {
//	        ctx.Abort(409, "OUT_OF_STOCK", "insufficient stock")
//	        return nil
//	    }
//	    return next()
//	}, maniflex.ForModel("Dispense"), maniflex.ForOperation(maniflex.OpCreate))
//
// When an ActionScope is in force, a record outside it is ErrNotFound and no
// lock is taken: FindByIDForUpdate is keyed by id alone and carries no filter,
// so the scope is applied by looking the record up through it first. The lookup
// runs inside the same transaction as the lock, so the row cannot leave the
// scope between the two.
func (c *ServerContext) LockForUpdate(modelName, id string) (map[string]any, error) {
	if c.Tx == nil {
		return nil, fmt.Errorf("maniflex: LockForUpdate requires an active transaction (ctx.Tx is nil)")
	}
	if c.reg == nil {
		return nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}
	meta, ok := c.reg.Get(modelName)
	if !ok {
		return nil, fmt.Errorf("maniflex: model %q is not registered", modelName)
	}
	if sf := c.scopeFilters(); len(sf) > 0 {
		if _, err := c.Tx.FindByID(c.Ctx, meta, id,
			&QueryParams{Page: 1, Limit: 1, Filters: sf}); err != nil {
			return nil, err
		}
	}
	v, err := c.Tx.FindByIDForUpdate(c.Ctx, meta, id)
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(meta, v), nil
}

// QueryModel reads records from any registered model using the standard
// FindMany path. When ctx.Tx is active the query participates in it.
// q may be nil — defaults to page 1, limit 20 with no filters or sorts.
//
// Deprecated: prefer ctx.GetModel(name).List(q) which also exposes Read,
// Create, Update, and Delete on the same accessor (3D.5).
func (c *ServerContext) QueryModel(modelName string, q *QueryParams) ([]map[string]any, error) {
	return c.GetModel(modelName).List(q)
}

// GetModel returns a ModelAccessor for the named registered model. All five
// CRUD operations on the accessor route through ctx.Tx when one is active.
// Returns an error accessor that surfaces the lookup failure on first use when
// the model name is not registered or the registry is unavailable.
//
// When an ActionScope is in force the accessor is scoped by it: reads see only
// matching records, a create is stamped with the scope's values, and an update
// or delete of a record outside it returns ErrNotFound — the same answer the
// scoped read gives. ctx.Unscoped().GetModel returns an unscoped accessor.
func (c *ServerContext) GetModel(modelName string) *ModelAccessor {
	return c.getModel(modelName, c.actionScope)
}

func (c *ServerContext) getModel(modelName string, scope *ActionScope) *ModelAccessor {
	if c.reg == nil {
		return &ModelAccessor{err: fmt.Errorf("maniflex: registry not available on this ServerContext")}
	}
	meta, ok := c.reg.Get(modelName)
	if !ok {
		return &ModelAccessor{err: fmt.Errorf("maniflex: model %q is not registered", modelName)}
	}
	// Route through the target model's adapter (per-model override takes
	// precedence) so cross-model reads/writes from ctx.GetModel hit the right DB.
	// When ctx.Tx is non-nil it was opened against the request's own adapter;
	// using it to access a model on a different adapter would be a bug, so we
	// only pass the tx through when the target shares the request adapter.
	targetAdapter := meta.ResolveAdapter(c.adapter)
	tx := c.Tx
	if tx != nil && targetAdapter != c.requestAdapter() {
		tx = nil
	}
	return &ModelAccessor{
		meta:        meta,
		exec:        dbExec{adapter: targetAdapter, tx: tx},
		ctx:         c.Ctx,
		keyProvider: c.keyProvider,
		scope:       scope,
	}
}

// ModelAccessor exposes the five standard CRUD operations for a single
// registered model. Obtain one via ServerContext.GetModel. All methods route
// through the active transaction (ctx.Tx) when one is set.
//
// When the accessor carries an ActionScope every method honours it — see
// action_scope.go. A nil scope is the ordinary case and costs nothing.
type ModelAccessor struct {
	meta        *ModelMeta
	exec        dbExec
	ctx         context.Context
	keyProvider KeyProvider  // encrypts/decrypts mfx:"encrypted" fields off the pipeline
	scope       *ActionScope // row-level scope from the Action's middleware; nil when unscoped
	err         error        // set when GetModel could not resolve the model
}

// scoped returns q with the accessor's scope AND-ed in, leaving the caller's q
// untouched. nil scope returns q as-is.
func (a *ModelAccessor) scoped(q *QueryParams) *QueryParams {
	if a.scope == nil || len(a.scope.Filters) == 0 {
		return q
	}
	if q == nil {
		q = &QueryParams{Page: 1, Limit: defaultLimit}
	}
	clone := *q
	clone.Filters = make([]*FilterExpr, 0, len(q.Filters)+len(a.scope.Filters))
	clone.Filters = append(clone.Filters, q.Filters...)
	clone.Filters = append(clone.Filters, a.scope.Filters...)
	return &clone
}

// inScope reports whether id names a record the scope admits. It is the write
// counterpart of the scoped read: the adapter's Update and Delete are keyed by
// id alone, so the only way to hold a write to a scope is to look the record up
// through it first. A miss is ErrNotFound — the same answer the scoped read
// gives for that id, so a caller learns nothing from the write they could not
// learn from the read.
func (a *ModelAccessor) inScope(id string) error {
	if a.scope == nil || len(a.scope.Filters) == 0 {
		return nil
	}
	_, err := a.exec.FindByID(a.ctx, a.meta, id,
		&QueryParams{Page: 1, Limit: 1, Filters: a.scope.Filters})
	return err
}

// List returns a page of records matching q. q may be nil (defaults to page 1,
// limit 20, no filters or sorts).
func (a *ModelAccessor) List(q *QueryParams) ([]map[string]any, error) {
	if a.err != nil {
		return nil, a.err
	}
	if q == nil {
		q = &QueryParams{Page: 1, Limit: defaultLimit}
	}
	rows, _, err := a.exec.FindMany(a.ctx, a.meta, a.scoped(q))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := decryptForRead(a.ctx, a.keyProvider, a.meta, row); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// Read returns the single record identified by id.
// Returns maniflex.ErrNotFound when the record does not exist.
func (a *ModelAccessor) Read(id string) (map[string]any, error) {
	if a.err != nil {
		return nil, a.err
	}
	row, err := a.exec.FindByID(a.ctx, a.meta, id, a.scoped(&QueryParams{}))
	if err != nil {
		return nil, err
	}
	if err := decryptForRead(a.ctx, a.keyProvider, a.meta, row); err != nil {
		return nil, err
	}
	return row, nil
}

// Create inserts a new record and returns the stored representation.
// Returns *maniflex.ErrConstraint on unique/check violations.
// A scope is stamped onto the record rather than checked: a create has no
// existing row to test, and a row created outside the scope would be invisible
// to the caller that made it. The stamp overwrites whatever the caller supplied
// for those columns, which is what stops a caller placing a row in someone
// else's scope — the same thing db.Tenancy does on the CRUD path.
func (a *ModelAccessor) Create(data map[string]any) (map[string]any, error) {
	if a.err != nil {
		return nil, a.err
	}
	inject, err := a.scope.injectable()
	if err != nil {
		return nil, err
	}
	if len(inject) > 0 {
		if data == nil {
			data = make(map[string]any, len(inject))
		}
		for col, v := range inject {
			data[col] = v
		}
	}
	if err := encryptForWrite(a.ctx, a.keyProvider, a.meta, data); err != nil {
		return nil, err
	}
	row, err := a.exec.Create(a.ctx, a.meta, data)
	if err != nil {
		return nil, err
	}
	if err := decryptForRead(a.ctx, a.keyProvider, a.meta, row); err != nil {
		return nil, err
	}
	return row, nil
}

// Update applies a partial patch to the record identified by id and returns
// the updated representation. Returns maniflex.ErrNotFound when absent.
// Returns *maniflex.ErrConstraint on unique/check violations.
// A scoped update of a record outside the scope returns ErrNotFound without
// writing.
func (a *ModelAccessor) Update(id string, data map[string]any) (map[string]any, error) {
	if a.err != nil {
		return nil, a.err
	}
	if err := a.inScope(id); err != nil {
		return nil, err
	}
	if err := encryptForWrite(a.ctx, a.keyProvider, a.meta, data); err != nil {
		return nil, err
	}
	row, err := a.exec.Update(a.ctx, a.meta, id, data)
	if err != nil {
		return nil, err
	}
	if err := decryptForRead(a.ctx, a.keyProvider, a.meta, row); err != nil {
		return nil, err
	}
	return row, nil
}

// Delete removes (or soft-deletes) the record identified by id.
// Returns maniflex.ErrNotFound when absent, and likewise when a scope is in
// force and the record falls outside it.
func (a *ModelAccessor) Delete(id string) error {
	if a.err != nil {
		return a.err
	}
	if err := a.inScope(id); err != nil {
		return err
	}
	return a.exec.Delete(a.ctx, a.meta, id)
}

// GoBackground schedules fn on a goroutine tracked by the Server. Server.Shutdown
// waits for tracked goroutines to complete (bounded by Config.ShutdownTimeout)
// so audit writes, cache invalidations, and other fire-and-forget work are not
// truncated by a process exit. The ctx passed to fn is independent of the HTTP
// request context (which is already returning when GoBackground is called) but
// IS cancelled when Shutdown's deadline elapses, so well-behaved writers honour
// that signal and exit promptly.
//
// When ctx.bg is nil (a synthesised ServerContext without server wiring), fn
// runs on a plain goroutine with context.Background() — no shutdown coupling.
func (c *ServerContext) GoBackground(fn func(context.Context)) {
	c.bg.Go(fn)
}

// Set stores a value that later pipeline steps or middleware can retrieve.
func (c *ServerContext) Set(key string, val any) {
	if c.values == nil {
		c.values = make(map[string]any)
	}
	c.values[key] = val
}

// Get retrieves a value stored by a previous pipeline step or middleware.
func (c *ServerContext) Get(key string) (any, bool) {
	if c.values == nil {
		return nil, false
	}
	v, ok := c.values[key]
	return v, ok
}

// ServiceName returns the Config.ServiceName configured on the Server instance,
// or "" when none was set. Middleware uses this to enrich audit records,
// outgoing requests, and observability payloads without holding a reference
// to the framework Config.
func (c *ServerContext) ServiceName() string {
	return c.serviceName
}

// HasRole reports whether the authenticated principal holds the given role.
func (c *ServerContext) HasRole(role string) bool {
	if c.Auth == nil {
		return false
	}
	for _, r := range c.Auth.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Logger returns a *slog.Logger pre-seeded with the request_id attribute so
// every log line emitted from middleware is automatically correlated to the
// originating HTTP request without any extra boilerplate.
//
//	ctx.Logger().Info("record processed", slog.String("id", record["id"].(string)))
//
// The returned logger is memoised across calls within the same request — the
// underlying attributes (service / request_id / trace_id) don't change once the
// request enters the pipeline, so per-step middleware can call Logger() in a
// hot path without paying a fresh base.With(...) on every emission.
//
// It is safe to call from any goroutine, including a ctx.GoBackground closure
// running after the request goroutine has moved on: the memoised logger is built
// exactly once, so every caller gets the same one.
func (c *ServerContext) Logger() *slog.Logger {
	c.loggerOnce.Do(func() { c.cachedLogger = c.buildLogger() })
	return c.cachedLogger
}

// buildLogger derives the request logger from the base logger plus the attributes
// that stay fixed for the request. Called once, under loggerOnce.
func (c *ServerContext) buildLogger() *slog.Logger {
	base := c.logger
	if base == nil {
		base = slog.Default()
	}
	var attrs []any
	if c.serviceName != "" {
		attrs = append(attrs, slog.String("service", c.serviceName))
	}
	if c.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", c.RequestID))
	}
	if c.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", c.TraceID))
	}
	if len(attrs) == 0 {
		return base
	}
	return base.With(attrs...)
}

// Abort populates ctx.Response with an error and should be used by middleware
// to short-circuit the pipeline. The current step must then return nil without
// calling next() for the abort to take effect.
func (c *ServerContext) Abort(statusCode int, code, message string) {
	c.Response = &APIResponse{
		StatusCode: statusCode,
		Error:      &APIError{Code: code, Message: message},
	}
	if c.trace != nil && c.trace.Aborts {
		_, file, line, ok := runtime.Caller(1)
		if ok {
			c.abortSite = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		} else {
			c.abortSite = "unknown"
		}
	}
}

// TrackStoredFile records an object written to FileStorage during this request
// so the framework can delete it (roll back the write) if the request ultimately
// fails. The default file-upload step calls this after every successful Store;
// custom Service middleware that writes to storage directly should call it too,
// passing the backend it used, to get the same orphan-cleanup guarantee (3B.2b).
//
// A request that ends 2xx keeps every tracked file. A pipeline error or a
// non-2xx final response deletes them all (see cleanupOrphanedFiles).
func (c *ServerContext) TrackStoredFile(key string, storage FileStorage) {
	if key == "" || storage == nil {
		return
	}
	c.storedFiles = append(c.storedFiles, storedFile{key: key, storage: storage})
}

// markStreamedFile records that the file field jsonName was stored during
// Deserialize by the upload:stream path, so the Service step skips it.
func (c *ServerContext) markStreamedFile(jsonName string) {
	if c.streamedFileFields == nil {
		c.streamedFileFields = make(map[string]struct{})
	}
	c.streamedFileFields[jsonName] = struct{}{}
}

// isStreamedFile reports whether jsonName was already stored during Deserialize
// by the upload:stream path.
func (c *ServerContext) isStreamedFile(jsonName string) bool {
	_, ok := c.streamedFileFields[jsonName]
	return ok
}

// cleanupOrphanedFiles deletes every object recorded by TrackStoredFile. The
// handler calls it when the request fails after the Service step stored one or
// more files (a pipeline error or a non-2xx final response). Deletion is
// best-effort and runs on the Server background runner so it neither blocks the
// error response nor is abandoned at shutdown. Draining storedFiles makes a
// second call a no-op.
func (c *ServerContext) cleanupOrphanedFiles() {
	if len(c.storedFiles) == 0 {
		return
	}
	orphans := c.storedFiles
	c.storedFiles = nil
	c.GoBackground(func(bgCtx context.Context) {
		for _, f := range orphans {
			_ = f.storage.Delete(bgCtx, f.key)
		}
	})
}

// trackReplacedFile records an existing storage object that a successful write
// this request will replace — an mfx:"auto_delete" file superseded or cleared on
// update. The object is deleted only when the request ends 2xx, via
// deleteReplacedFiles; deferring the delete past the DB write means a failed
// update never deletes a blob the (unchanged) row still references (BUG-1).
func (c *ServerContext) trackReplacedFile(key string, storage FileStorage) {
	if key == "" || storage == nil {
		return
	}
	c.replacedFiles = append(c.replacedFiles, storedFile{key: key, storage: storage})
}

// deleteReplacedFiles deletes every object recorded by trackReplacedFile. The
// handler calls it once the request has ended 2xx, so the old blobs a successful
// update superseded are removed. Deletion is best-effort on the Server background
// runner so it neither blocks the response nor is abandoned at shutdown; a
// missing object is not an error. Draining makes a second call a no-op.
func (c *ServerContext) deleteReplacedFiles() {
	if len(c.replacedFiles) == 0 {
		return
	}
	replaced := c.replacedFiles
	c.replacedFiles = nil
	c.GoBackground(func(bgCtx context.Context) {
		for _, f := range replaced {
			_ = f.storage.Delete(bgCtx, f.key)
		}
	})
}

// ── Transaction tracing ───────────────────────────────────────────────────────

// tracedTx wraps a Tx and logs Commit and Rollback calls at DEBUG level.
// It is only used when Config.Trace.Steps is true.
type tracedTx struct {
	Tx
	ctx *ServerContext
}

// sqlExecContexter mirrors events.SQLExecer without importing the events package.
type sqlExecContexter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (t *tracedTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if ex, ok := t.Tx.(sqlExecContexter); ok {
		return ex.ExecContext(ctx, query, args...)
	}
	return nil, fmt.Errorf("maniflex: underlying Tx does not implement ExecContext")
}

// RawQueryContext / RawExecContext forward the rawableT contract to the wrapped
// Tx. Embedding the Tx interface promotes only the methods Tx declares, so
// without these a traced transaction would not satisfy rawableT — and turning on
// Config.Trace would quietly push every ctx.RawQuery/RawExec out of the
// transaction and onto the bare adapter (BUG-12).
func (t *tracedTx) RawQueryContext(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rt, ok := t.Tx.(rawableT)
	if !ok {
		return nil, ErrRawNotSupportedInTx
	}
	return rt.RawQueryContext(ctx, query, args...)
}

func (t *tracedTx) RawExecContext(ctx context.Context, query string, args ...any) (int64, error) {
	rt, ok := t.Tx.(rawableT)
	if !ok {
		return 0, ErrRawNotSupportedInTx
	}
	return rt.RawExecContext(ctx, query, args...)
}

func (t *tracedTx) Commit() error {
	err := t.Tx.Commit()
	if err != nil {
		t.ctx.Logger().Debug("transaction commit failed", slog.String("error", err.Error()))
	} else {
		t.ctx.Logger().Debug("transaction commit")
	}
	return err
}

func (t *tracedTx) Rollback() error {
	err := t.Tx.Rollback()
	// Rollback after a successful commit returns sql.ErrTxDone — log only
	// genuine rollbacks (err == nil means the rollback itself succeeded).
	if err == nil {
		t.ctx.Logger().Debug("transaction rollback")
	}
	return err
}

// NewBackground constructs a minimal ServerContext for use outside the HTTP
// pipeline — background workers, tests, and CLI commands. The returned context
// has no HTTP request wired up; operations that require ctx.Request will panic.
//
// Example (tests):
//
//	bgCtx := maniflex.NewBackground(context.Background(), srv.DB(), srv.Registry())
//	entry, err := l.Post(bgCtx, ...)
func NewBackground(ctx context.Context, adapter DBAdapter, reg RegistryAccessor) *ServerContext {
	return &ServerContext{
		Ctx:     ctx,
		adapter: adapter,
		reg:     reg,
	}
}

// SetKeyProvider wires the KeyProvider used to encrypt/decrypt mfx:"encrypted"
// fields on the typed (maniflex.Create/Read) and ctx.GetModel paths. The HTTP
// pipeline sets this automatically; call it on a NewBackground context (workers,
// CLIs) that reads or writes encrypted models, e.g.
//
//	bg := maniflex.NewBackground(ctx, srv.DB(), srv.Registry())
//	bg.SetKeyProvider(srv.KeyProvider())
func (c *ServerContext) SetKeyProvider(kp KeyProvider) { c.keyProvider = kp }

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrNoAdapter is returned by BeginTx when no database adapter has been
// configured on the Server instance.
var ErrNoAdapter = errNoAdapter("maniflex: no database adapter configured")

// ErrRawNotSupportedInTx is returned by RawQuery and RawExec when a transaction
// is active but its Tx implementation cannot run raw SQL. The statement is
// refused rather than run on the bare adapter: that would put it on a different
// connection, outside the transaction, where it would commit on its own and
// survive the rollback the caller believes covers it (BUG-12).
var ErrRawNotSupportedInTx = errors.New(
	"maniflex: the active transaction does not support raw SQL; " +
		"RawQuery/RawExec would run outside it")

type errNoAdapter string

func (e errNoAdapter) Error() string { return string(e) }

// ── Response types ────────────────────────────────────────────────────────────

// APIResponse is the structured response written to the wire by the
// Response pipeline step.
type APIResponse struct {
	StatusCode int
	Data       any
	Error      *APIError
	Meta       *ResponseMeta
	AsIs       bool
	// Dir, when non-empty, adds {"_dir": Dir} to the response meta. For list
	// responses Meta already carries Dir via ResponseMeta.Dir; this field is
	// used for single-record responses (read/create/update) that have no
	// pagination meta — it produces {"data": ..., "meta": {"_dir": "rtl"}}.
	Dir string
}

// APIError is the error body for non-2xx responses.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// ResponseMeta carries pagination metadata for list responses. It serialises in
// one of two shapes depending on the pagination mode (see MarshalJSON):
// offset mode emits {total, page, limit, pages}; cursor (keyset) mode emits
// {limit, next_cursor, has_more}.
type ResponseMeta struct {
	Total int64  `json:"total"`
	Page  int    `json:"page"`
	Limit int    `json:"limit"`
	Pages int64  `json:"pages"`
	Dir   string `json:"_dir,omitempty"` // "rtl" when the active locale uses right-to-left script

	// Cursor-mode fields. Cursor is set to render the keyset shape; NextCursor is
	// the token for the following page ("" on the last page).
	Cursor     bool   `json:"-"`
	NextCursor string `json:"-"`
	HasMore    bool   `json:"-"`
}

// MarshalJSON renders the offset shape ({total, page, limit, pages}) by default,
// and the cursor shape ({limit, next_cursor, has_more}) when Cursor is set, so a
// keyset response never carries meaningless total/page/pages fields.
func (m ResponseMeta) MarshalJSON() ([]byte, error) {
	if m.Cursor {
		return json.Marshal(struct {
			Limit      int    `json:"limit"`
			NextCursor string `json:"next_cursor,omitempty"`
			HasMore    bool   `json:"has_more"`
			Dir        string `json:"_dir,omitempty"`
		}{m.Limit, m.NextCursor, m.HasMore, m.Dir})
	}
	return json.Marshal(struct {
		Total int64  `json:"total"`
		Page  int    `json:"page"`
		Limit int    `json:"limit"`
		Pages int64  `json:"pages"`
		Dir   string `json:"_dir,omitempty"`
	}{m.Total, m.Page, m.Limit, m.Pages, m.Dir})
}

// Write serialises the APIResponse and sends it to the HTTP ResponseWriter.
func (r *APIResponse) Write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if r.StatusCode == 0 {
		r.StatusCode = http.StatusOK
	}
	if r.StatusCode != http.StatusOK {
		w.WriteHeader(r.StatusCode)
	}

	if r.StatusCode == http.StatusNoContent {
		return
	}

	var body any
	if r.AsIs {
		if r.Data == nil {
			return
		}
		body = r.Data
	} else {
		switch {
		case r.Error != nil:
			body = map[string]any{"error": r.Error}
		case r.Meta != nil:
			body = map[string]any{"data": r.Data, "meta": r.Meta}
		case r.Dir != "":
			body = map[string]any{"data": r.Data, "meta": map[string]any{"_dir": r.Dir}}
		default:
			body = map[string]any{"data": r.Data}
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}
