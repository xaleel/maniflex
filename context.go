package maniflex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

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
	OpHead    Operation = "head"

	// OpReadAttachment identifies a request for GET /:model/:id/:file_field —
	// the per-model attachment route. Runs Auth → Deserialize → Validate → DB
	// (FindByID) like a regular Read, then the Response step streams the
	// referenced file instead of writing a JSON envelope.
	//
	// Middleware filtered with ForOperation(OpRead) does NOT match attachment
	// requests; use ForOperation(OpRead, OpReadAttachment) to apply to both.
	OpReadAttachment Operation = "read_attachment"

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
	// Middleware filtered with ForOperation(OpList) does NOT match exports;
	// use ForOperation(OpList, OpExport) to cover both.
	OpExport Operation = "export"
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
	cachedLogger *slog.Logger

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
}

// storedFile pairs an object key with the storage backend it was written to, so
// orphan cleanup can delete it without assuming a single global FileStorage.
type storedFile struct {
	key     string
	storage FileStorage
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
	// Read one byte past the limit so a body of exactly maxBodyBytes is accepted
	// while anything larger is detected rather than silently truncated into a
	// confusing INVALID_JSON.
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		c.Abort(http.StatusBadRequest, "BODY_READ_ERROR", "failed to read request body")
		return false, fmt.Errorf("body read: %w", err)
	}
	if int64(len(body)) > maxBodyBytes {
		c.Abort(http.StatusBadRequest, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %s limit", formatByteSize(maxBodyBytes)))
		return false, fmt.Errorf("body exceeds %d bytes", maxBodyBytes)
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
func (c *ServerContext) QueryParam(name string) string {
	return c.Request.URL.Query().Get(name)
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

func (c *ServerContext) BeginTx(ctx context.Context, opts *TxOptions) (Tx, error) {
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
// Placeholders are rebound to the adapter's dialect, so `?` works on both SQLite
// and Postgres ($N). Never interpolate values directly into the query string.
func (c *ServerContext) RawQuery(query string, args ...any) ([]Row, error) {
	if rt, ok := c.Tx.(rawableT); ok {
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
func (c *ServerContext) RawExec(query string, args ...any) (int64, error) {
	if rt, ok := c.Tx.(rawableT); ok {
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
// the enclosing transaction commits or rolls back. For SQLite the lock is
// implicit in the transaction isolation level (use BEGIN IMMEDIATE via
// TxOptions or the _txlock=immediate DSN option).
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
func (c *ServerContext) GetModel(modelName string) *ModelAccessor {
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
	}
}

// ModelAccessor exposes the five standard CRUD operations for a single
// registered model. Obtain one via ServerContext.GetModel. All methods route
// through the active transaction (ctx.Tx) when one is set.
type ModelAccessor struct {
	meta        *ModelMeta
	exec        dbExec
	ctx         context.Context
	keyProvider KeyProvider // encrypts/decrypts mfx:"encrypted" fields off the pipeline
	err         error       // set when GetModel could not resolve the model
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
	rows, _, err := a.exec.FindMany(a.ctx, a.meta, q)
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
	row, err := a.exec.FindByID(a.ctx, a.meta, id, &QueryParams{})
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
func (a *ModelAccessor) Create(data map[string]any) (map[string]any, error) {
	if a.err != nil {
		return nil, a.err
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
func (a *ModelAccessor) Update(id string, data map[string]any) (map[string]any, error) {
	if a.err != nil {
		return nil, a.err
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
// Returns maniflex.ErrNotFound when absent.
func (a *ModelAccessor) Delete(id string) error {
	if a.err != nil {
		return a.err
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
func (c *ServerContext) Logger() *slog.Logger {
	if c.cachedLogger != nil {
		return c.cachedLogger
	}
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
		c.cachedLogger = base
	} else {
		c.cachedLogger = base.With(attrs...)
	}
	return c.cachedLogger
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
