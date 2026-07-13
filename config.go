package maniflex

import (
	"context"
	"log/slog"
	"time"
)

// SoftDeleteStyle indicates how soft deletion is stored in the database.
type SoftDeleteStyle int

const (
	// SoftDeleteTimestamp stores a nullable timestamp; NULL means not deleted.
	SoftDeleteTimestamp SoftDeleteStyle = iota
	// SoftDeleteBool stores a boolean flag; false/0 means not deleted.
	SoftDeleteBool
)

// SoftDeleteConfig describes how a model handles soft deletion.
// When Enabled is false (default) hard-deletes are performed.
type SoftDeleteConfig struct {
	Enabled   bool
	Field     string          // DB column name, e.g. "deleted_at"
	FieldType SoftDeleteStyle // how to test "is deleted"
}

// ModelMiddleware holds per-step middleware registered alongside a model at
// registration time. Every middleware is implicitly scoped to the model it is
// registered with; adding a ForModel option is not required and would be
// redundant.
//
// Used by jobs/maniflex.Mount to install write-blockers and force-filters without
// requiring a separate server.Pipeline.X.Register call after registration.
type ModelMiddleware struct {
	Auth        []MiddlewareFunc
	Deserialize []MiddlewareFunc
	Validate    []MiddlewareFunc
	Service     []MiddlewareFunc
	DB          []MiddlewareFunc
	Response    []MiddlewareFunc
}

// ModelConfig holds user-supplied options for a single registered model.
// All fields are optional; sensible defaults are derived from the struct name.
type ModelConfig struct {
	// TableName overrides the auto-generated snake_case plural table name.
	TableName string
	// SoftDelete opts the model into soft deletion.
	SoftDelete SoftDeleteConfig
	// Middleware holds per-model pipeline middleware installed at registration
	// time. nil means no per-model middleware.
	Middleware *ModelMiddleware

	// Versioned enables field-change history for this model. AutoMigrate
	// creates a sibling {model}_history table. Every write emits a history row
	// with a per-field diff. Equivalent to mfx:"versioned" on BaseModel.
	Versioned bool
	// VersionedDiffOnly skips the snapshot column. Only changed fields are
	// stored. Equivalent to mfx:"versioned:diff_only" on BaseModel.
	VersionedDiffOnly bool

	// Indices declares extra DB indexes to create during AutoMigrate. Use this
	// to pre-declare indexes that the framework would otherwise auto-generate
	// (e.g. for mfx:"scheduled" timestamp columns) so duplicates are skipped.
	Indices []IndexSpec

	// Adapter overrides Config.DB for this model only. When non-nil, all CRUD
	// reads/writes, AutoMigrate, and transactions for this model route through
	// it instead of the global Config.DB.
	//
	// Leave nil to use Config.DB. Used to spread aggregates across separate
	// databases (e.g. orders DB vs. inventory DB) without running multiple
	// service binaries.
	//
	// maniflex.Batch and pkg/saga cannot span adapters in a single transaction —
	// Batch construction rejects mixed-adapter model sets; sagas are the
	// supported cross-adapter pattern.
	Adapter DBAdapter

	// ExportEnabled mounts GET /:model/export when true. The route accepts the
	// same filter and sort query parameters as the standard list endpoint and
	// streams the result as CSV (default) or XLSX (?format=xlsx). Hidden and
	// writeonly fields are excluded from the output.
	ExportEnabled bool

	// MaxExportRows caps the number of rows the export endpoint will return.
	// 0 means use DefaultMaxExportRows (100,000). Exports that would exceed
	// the cap return 413 Request Entity Too Large.
	MaxExportRows int

	// CursorField opts the model into keyset (cursor) pagination and names the
	// column the ?cursor= walk orders by. Value is the JSON or DB name of the
	// field (resolved to a DB column at registration); empty leaves the model on
	// offset (?page=/?limit=) pagination only. Equivalent to declaring
	// mfx:"cursor_field:<name>" on a field. The cursor field should be indexed
	// and effectively monotonic (e.g. created_at, a sequence) for best results;
	// id is always appended as the tiebreaker so the keyset boundary is total.
	CursorField string

	// AggregateEnabled mounts GET /:model/aggregate when true. The route accepts
	// a JSON body describing an aggregation (select/group_by/where/having/
	// order_by/limit), validates every referenced field against the model's
	// filterable/sortable allow-list, and runs it through ctx.Aggregate. It
	// dispatches as the list operation, so auth and row-isolation middleware
	// registered for OpList apply unchanged and any ?filter= query parameters
	// (including tenancy force-filters) are AND-ed into the aggregate WHERE.
	//
	// Field and operator names use the same convention as ?filter=/?sort=:
	// the JSON field name (DB column name also accepted). Only count, sum, avg,
	// min, max, and count_distinct are exposed. The result is returned as a
	// JSON array of group rows under the usual {"data": ...} envelope.
	AggregateEnabled bool

	// SearchLanguage names the text-search configuration used for full-text
	// search (?q=) on mfx:"searchable" fields. On Postgres it is the
	// to_tsvector / websearch_to_tsquery configuration name (default "english");
	// on SQLite it is ignored (the FTS5 porter tokenizer is language-agnostic).
	// The value is embedded into SQL as a config identifier — it must be a plain
	// identifier ([A-Za-z_]+) and is rejected at registration otherwise. Empty
	// means the framework default ("english").
	SearchLanguage string

	// GlobalSearchable opts the model into the built-in cross-model search
	// endpoint (GET /search, enabled via Server.EnableGlobalSearch). Only models
	// with this flag are searched by that endpoint and may be named in its
	// ?models= filter. It requires the model to declare at least one
	// mfx:"searchable" field — registration fails otherwise. It has no bearing on
	// per-model ?q= search (that needs only mfx:"searchable"), nor on ctx.Search
	// called with an explicit model list (the Action-scoped path, which the app
	// authorises itself).
	GlobalSearchable bool

	// DefaultLocale is the model-level fallback locale for LocaleString fields
	// in resolve/split mode when the client did not request a specific locale
	// and the field has no default_locale tag. Falls back to
	// LocaleOptions.Default when empty.
	DefaultLocale string

	// DefaultLocaleMode sets the default response representation for all
	// LocaleString fields on this model that do not carry an explicit mode tag.
	// When empty the app-level LocaleOptions.DefaultLocaleMode applies, then
	// the framework default (split).
	DefaultLocaleMode LocaleMode

	// OptimisticLock enables If-Match / ETag concurrency control for PATCH and
	// DELETE operations. When set, the DB step fetches the current record before
	// executing the write, computes its ETag (MD5 of the JSON response body),
	// and returns 412 Precondition Failed if the If-Match header does not match.
	//
	// The ETag format is identical to the one emitted by response.Cache, so
	// clients can obtain it via a preceding GET and use it on the mutating
	// request without special handling.
	//
	// If-Match: * is the RFC 9110 wildcard — it holds for any existing record,
	// so it means "overwrite whatever is there, but do not create it" rather
	// than pinning a particular version.
	//
	// Requests that omit the If-Match header bypass the check — the field
	// opts in to enforcement, not to mandatory locking.
	OptimisticLock bool

	// Singleton turns the model into a single-row config / feature-flag
	// resource. Instead of the usual collection + item routes, the model mounts
	// only GET and PATCH on its bare table path (no id, no POST/DELETE/list):
	//
	//	GET   /{table}   → read the one row
	//	PATCH /{table}   → update the one row
	//
	// The row is provisioned lazily under the well-known SingletonID on first
	// access, so GET returns column defaults before anything is written and
	// PATCH always targets an existing row. This is the blessed shape for the
	// "admin edits one config record, clients read it at launch" pattern (e.g.
	// GET /config). Singleton models must not declare mfx:"required" fields —
	// the auto-provisioned row has no values to satisfy them.
	Singleton bool

	// Headless registers the model fully — migration, registry, typed access,
	// relations — but mounts NO REST routes for it. Use it to back a path with a
	// custom server.Action instead of the auto-generated CRUD: a model and an
	// action cannot both own the same method+path (chi panics at boot), so set
	// Headless on the model to free its table path (e.g. GET /threads) for the
	// action. The model is still reachable via ctx.GetModel / typed CRUD and via
	// relations from other models. Takes precedence over Singleton.
	Headless bool
}

// SingletonID is the fixed primary key of the single row backing a
// ModelConfig.Singleton model. The row is created with this id on first access
// and every GET/PATCH on the model addresses it.
const SingletonID = "singleton"

// DefaultMaxExportRows is the row cap used when ModelConfig.MaxExportRows
// is unset.
const DefaultMaxExportRows = 100_000

// Config is the top-level configuration passed to New().
type Config struct {
	// Port the HTTP server listens on. Default: 8080.
	Port int

	// PathPrefix is prepended to every generated route. Default: "/api".
	PathPrefix string

	// StaticDir is the filesystem directory served as static files. A relative
	// path is resolved against the working directory. When empty it defaults to
	// "<cwd>/static". If the directory does not exist it is skipped with a
	// warning (no error). Ignored when StaticDisabled is true.
	StaticDir string

	// StaticPrefix is the URL path prefix under which StaticDir is served.
	// Default: "/static". Unlike model routes it is mounted at the router root,
	// NOT under PathPrefix, preserving the historical "<cwd>/static → /static"
	// mapping.
	StaticPrefix string

	// StaticDisabled turns off static file serving entirely, even when the
	// directory exists. Use it when "./static" is present for other reasons and
	// must not be exposed over HTTP.
	StaticDisabled bool

	// TrustProxyHeaders controls whether the client IP is derived from the
	// X-Forwarded-For / X-Real-IP request headers (via chi's RealIP middleware).
	// It is OFF by default: RemoteAddr stays the real TCP peer, so a client
	// cannot forge its own address.
	//
	// Every IP-keyed feature reads the resolved RemoteAddr — per-IP rate limiting
	// (db.RateLimit / db.RateLimitAction), idempotency scoping, and read-audit
	// records. With this flag off they key on the direct peer; with it on they
	// key on the forwarded client IP.
	//
	// Enable it ONLY when the server sits behind a trusted reverse proxy or load
	// balancer that (a) sets X-Forwarded-For to the real client and (b) strips any
	// inbound XFF sent by the client. Turning it on while directly internet-facing
	// lets an attacker spoof its address with an X-Forwarded-For header, defeating
	// per-IP rate limits and poisoning audit logs (SEC-5).
	TrustProxyHeaders bool

	// ServiceName identifies this service in logs, audit records, and outgoing
	// requests. When set:
	//   - every framework log line gains a "service" attribute,
	//   - every audit record's ServiceName field is populated,
	//   - the X-Service-Name response header is set on every response.
	// When empty (the default) none of the above happens — zero behavioural
	// change for callers that don't configure it.
	ServiceName string

	// DB is the database adapter to use. Required before calling Start().
	DB DBAdapter

	// DisableAutoMigrate turns off schema creation/migration on Start() and
	// MigrateOnly(). Migration runs by default; set this to true to skip it (e.g.
	// when migrations are managed out of band). Replaces the old AutoMigrate bool,
	// whose zero value (false) could not honour the documented "default on".
	DisableAutoMigrate bool

	// ShutdownTimeout is the maximum duration Start() waits for in-flight
	// requests to complete after a shutdown signal is received before forcing
	// connections closed. Default: 30s.
	//
	// Set a shorter value for fast-cycling environments (e.g. tests, lambdas),
	// and a longer value when requests may take several seconds (e.g. bulk
	// imports, large file uploads).
	ShutdownTimeout time.Duration

	// Logger is the slog.Logger used for all framework-level log output:
	// server lifecycle messages (startup, shutdown, migration), per-request
	// logs emitted via ServerContext.Logger(), and DB adapter messages such as
	// AutoMigrate column-drift warnings.
	//
	// When nil, slog.Default() is used, which writes plain-text lines to
	// stderr. Set an explicit logger to route output to a JSON handler, a
	// remote aggregator, or a test capture handler.
	Logger *slog.Logger

	// PanicLogger is the slog.Logger used by the panic recovery middleware.
	// Every unhandled panic is logged at ERROR level with structured fields:
	// method, path, request_id, panic value, and full stack trace.
	//
	// When nil, Logger is used (falling back to slog.Default() if Logger is
	// also nil). Set PanicLogger explicitly only when panics must be routed
	// to a different sink than the rest of the framework logs.
	PanicLogger *slog.Logger

	// Trace configures pipeline tracing for debug purposes.
	// Set Trace.Enabled to true to activate all standard trace output (step
	// enter/exit, timings, and abort call sites). Individual sub-flags allow
	// finer control — see PipelineTrace for details.
	//
	// All trace output is emitted at DEBUG level through Config.Logger, so it
	// is invisible unless the logger's handler accepts DEBUG records.
	Trace PipelineTrace

	// QueryTimeout is the maximum duration allowed for a single request's
	// database operations. When non-zero a context.WithTimeout deadline is
	// attached to ServerContext.Ctx before the pipeline runs, so every DB adapter
	// call made during the request — including calls from middleware — inherits
	// the same deadline.
	//
	// If a query exceeds this deadline the DB step returns HTTP 504 with error
	// code "TIMEOUT" instead of the usual 500.
	//
	// Zero (the default) means no per-request timeout is applied; ctx.Ctx
	// carries the HTTP request context as-is, which has no deadline unless the
	// client disconnects.
	//
	// Typical values:
	//   5s  — tight APIs where every response must be fast
	//   30s — general OLTP; catches runaway queries without affecting normal use
	//   0   — no timeout (legacy / migration path)
	QueryTimeout time.Duration

	// HealthCheckDB controls whether GET /health pings the database.
	//
	// When false (the default) the endpoint always returns {"status":"ok"} with
	// HTTP 200 — matching the previous behaviour and suitable for liveness
	// probes that only need to know the process is alive.
	//
	// When true the handler calls db.Ping() with a HealthTimeout deadline:
	//   - On success: HTTP 200  {"status":"ok",      "db":"ok"}
	//   - On failure: HTTP 503  {"status":"degraded","db":"error","error":"..."}
	//
	// Enable this for Kubernetes readiness probes so the pod is only marked
	// ready once it can actually reach the database.
	HealthCheckDB bool

	// DBWriteURL is the DSN/connection-string for the primary (write) database.
	// Not used by the framework directly — set Config.DB with the adapter you
	// open from this URL. Populated by ConfigFromEnv.
	DBWriteURL string

	// DBReadURL is the DSN/connection-string for the read replica. Pass an empty
	// string to route reads to the write primary. Populated by ConfigFromEnv.
	DBReadURL string

	FilesConfig FilesConfig

	// KeyProvider is the encryption key provider for mfx:"encrypted" fields.
	// When nil, any attempt to create or update a record with encrypted fields
	// will be rejected with HTTP 500 ENCRYPTION_NOT_CONFIGURED.
	// Reads of already-encrypted values return the raw stored ciphertext string.
	//
	// Use pkg/encryption.EnvKeyProvider for keys in environment variables, or
	// pkg/encryption.VaultKeyProvider for HashiCorp Vault Transit.
	KeyProvider KeyProvider

	// HealthTimeout is the maximum time the health handler waits for the
	// database ping to complete. Only used when HealthCheckDB is true.
	// Default: 3s.
	//
	// Choose a value smaller than your probe's timeoutSeconds so the handler
	// can return a clean 503 before the probe itself times out:
	//
	//   readinessProbe:
	//     httpGet:
	//       path: /api/health
	//     timeoutSeconds: 5        # probe timeout
	//   # → set HealthTimeout to 3s so 503 arrives before 5s probe timeout
	HealthTimeout time.Duration

	// OnStart is a lightweight lifecycle hook run once during boot, after
	// migration and DB-ready but before the HTTP listener opens — the same slot
	// as Service.Start, and ahead of any registered services. A non-nil error
	// aborts boot exactly like a failed migration. The ctx is cancelled when
	// shutdown begins.
	//
	// Use it for callers that want a start hook without defining a Service type;
	// for components that also need an ordered Stop, register a Service instead.
	OnStart func(ctx context.Context) error

	// OnShutdown is the symmetric hook run once during graceful shutdown, after
	// the HTTP listener has drained and all services have stopped. The ctx is a
	// fresh deadline context bounded by ShutdownTimeout. A returned error is
	// logged but does not change the shutdown outcome.
	OnShutdown func(ctx context.Context) error
}

// PipelineTrace controls per-request debug tracing through the middleware pipeline.
// All output is at DEBUG level; set Config.Logger to a handler that accepts
// DEBUG records to see it.
type PipelineTrace struct {
	// Enabled is a shorthand that activates Steps, Timings, and Aborts when set
	// to true without any sub-flags being explicitly configured.
	// Bodies and Skips are NOT turned on by Enabled — they are opt-in because
	// they are high-volume or may expose sensitive request data.
	Enabled bool

	// Steps logs an "enter" record before each named middleware runs and an
	// "exit" record after it returns, with step name and middleware name.
	Steps bool

	// Timings adds an elapsed duration to each "exit" record. Requires Steps.
	Timings bool

	// Aborts logs when ctx.Abort() is called: the HTTP status, error code, and
	// the source file:line of the Abort call site inside the middleware.
	Aborts bool

	// Bodies logs the field names present in ctx.ParsedBody after the Deserialize
	// step. WARNING: may expose sensitive fields (passwords, tokens). Disabled by
	// Enabled; must be set explicitly.
	Bodies bool

	// Skips logs when a registered middleware is skipped because its ForModel or
	// ForOperation filter did not match the current request.
	Skips bool
}

func (c *Config) ApplyDefaults() {
	if c.Port == 0 {
		c.Port = 8080
	}
	if c.PathPrefix == "" {
		c.PathPrefix = "/api"
	}
	if c.StaticPrefix == "" {
		c.StaticPrefix = "/static"
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	// PanicLogger falls back to Logger so callers only need to set one field.
	if c.PanicLogger == nil {
		c.PanicLogger = c.Logger // PanicRecoverer accepts nil and uses slog.Default()
	}
	// Expand Trace.Enabled into standard sub-flags when none are set explicitly.
	if c.Trace.Enabled && !c.Trace.Steps && !c.Trace.Timings && !c.Trace.Aborts {
		c.Trace.Steps = true
		c.Trace.Timings = true
		c.Trace.Aborts = true
	}
	if c.HealthCheckDB && c.HealthTimeout == 0 {
		c.HealthTimeout = 3 * time.Second
	}
	// QueryTimeout intentionally has no default — zero means disabled.
	// Users opt in explicitly so a misconfigured timeout does not silently
	// break long-running imports or migrations.
}

// logger returns the configured Logger, or slog.Default() when nil.
func (c *Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// traceConfig returns a pointer to the active PipelineTrace when any flag is
// set, or nil when all flags are false. The nil check in hot paths avoids any
// overhead when tracing is disabled.
func (c *Config) traceConfig() *PipelineTrace {
	tr := &c.Trace
	if !tr.Steps && !tr.Timings && !tr.Aborts && !tr.Bodies && !tr.Skips {
		return nil
	}
	return tr
}
