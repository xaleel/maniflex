package maniflex

// execute.go — running the pipeline from Go rather than from a socket (R3).
//
// Until this existed, HTTP was the only way in. An app that needed to run a
// registered model's operation from its own code — a maker-checker replay, a
// saga step, a seeder — had to make an HTTP request to itself and, since a
// router takes no principal argument, say who it was in a header. Both halves of
// that are broken, and not by carelessness:
//
//   - A principal passed as a header is a principal any client can also send. The
//     gates then grow bypasses keyed on that header, and the bypass is reachable
//     from the internet by construction. This is not a bug to be fixed with care;
//     it is what the shape of the interface asks for.
//   - N requests cannot be one transaction. A loop that replays N items over HTTP
//     commits each one separately, so a failure at item 3 leaves 1 and 2 written
//     and the batch marked unfinished — and re-running it re-applies the prefix.
//
// Execute deletes both. Auth is a typed *AuthInfo, so there is no header to
// forge and no bypass to grow. Tx is the caller's transaction, so N invocations
// commit or roll back together.
//
// The pipeline is not reimplemented here, and that is the whole design. Execute
// synthesises the *http.Request the steps already know how to read and runs the
// same six-step chain a real request runs, in the same order, through the same
// middleware. Nothing gets a second code path to drift from the first: the body
// is bound by the real Deserialize step, so PATCH presence semantics (ctx.present
// — which decides the difference between "set this column to null" and "leave it
// alone") are the ones the HTTP path has, rather than a re-derivation of them
// that agrees today and diverges next year.
//
// There is deliberately no way to skip a step. The request that prompted this
// asked for one — Steps: StepsValidate|StepsService|StepsDB, skipping Auth — and
// that would have re-created the vulnerability it was meant to remove: force
// filters are registered on the Auth step (maniflex's own jobs/maniflex does it),
// so skipping Auth to inject a principal skips the isolation that principal is
// supposed to be constrained by. Auth runs. What changes is that the identity
// middleware can see the principal is already established — see InProcess.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Invocation describes one pipeline run raised from Go. See Server.Execute.
type Invocation struct {
	// Model is the registered model's struct name, e.g. "Order". Required.
	Model string

	// Operation is the operation to run: OpCreate, OpRead, OpUpdate, OpDelete or
	// OpList. Required. The streaming operations (OpExport, OpReadAttachment) and
	// the trimmed-pipeline ones (OpAction, OpSearch) are refused — see Execute.
	Operation Operation

	// ID is the record's primary key, for OpRead, OpUpdate and OpDelete.
	ID string

	// Body is the request body for OpCreate and OpUpdate. A []byte or
	// json.RawMessage is used verbatim; anything else is marshalled. For an
	// OpUpdate this is what decides presence: a field absent from the body is
	// left alone, exactly as it is over HTTP, so send a map when you mean to
	// patch some columns and a full struct when you mean to send them all.
	Body any

	// Query carries what a URL's query string would: filters, sort, include,
	// pagination. Build it with url.Values, or leave it nil.
	//
	//	Query: url.Values{"filter": {"status:eq:open"}, "limit": {"50"}},
	Query url.Values

	// Auth is the principal this invocation runs as. It is a typed value rather
	// than a header, which is the point: a caller cannot forge it and a client
	// cannot reach it. Leave it nil to run unauthenticated.
	Auth *AuthInfo

	// Tx, when set, is the transaction this invocation joins. Several Executes
	// sharing one Tx commit or roll back together, which is what makes a
	// multi-item staged write atomic. The caller owns it: Execute neither commits
	// nor rolls back.
	Tx Tx

	// Header carries request headers for middleware that reads them (If-Match,
	// Accept-Language, Idempotency-Key). Authorization does not belong here — use
	// Auth. Optional.
	Header http.Header
}

// ExecuteError is returned by Server.Execute when the pipeline answered with a
// status outside 2xx. The full response is on Response.
//
// A non-2xx is an error rather than a value on purpose. The batch this exists to
// serve loops N invocations inside one transaction, and the failure it must
// survive is item 3 of 5 answering 422 — so the natural Go loop, `if err != nil
// { return err }`, has to be the one that rolls back. Handing a 422 back as
// (res, nil) would make the naive loop commit the prefix, which is the exact bug
// the caller came here to fix. Use errors.As to inspect the status.
type ExecuteError struct {
	// StatusCode is the HTTP status the pipeline produced.
	StatusCode int

	// Code and Message are the API error's code and message, when it carried one.
	Code    string
	Message string

	// Response is the whole envelope, including any Data.
	Response *APIResponse
}

func (e *ExecuteError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("maniflex: execute: status %d", e.StatusCode)
	}
	return fmt.Sprintf("maniflex: execute: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// InProcess reports whether this request was raised by Server.Execute rather
// than sent by a client.
//
// It exists because a synthesised request is, to a middleware, indistinguishable
// from a real one — which is what makes Execute cheap and what makes it worth
// asking about. Middleware whose job is transport-shaped can consult it and step
// aside: there is no browser to protect from CSRF, no remote address to rate-limit
// per, and no Authorization header to read when the principal arrived as a typed
// value. The shipped auth.JWTAuth/JWKSAuth do exactly the last of these.
//
// It is derived from an unexported field, so only Execute can make it true. That
// is what separates it from the header it replaces: a client can send any header
// it likes, and cannot reach this at all.
func (c *ServerContext) InProcess() bool { return c.inProcess }

// IsRestore reports whether this request is a soft-delete restore
// (POST /:model/:id/restore), which dispatches as OpUpdate so that existing
// update middleware applies to it.
//
// Middleware registered with ForOperation(OpUpdate) runs for restores too, and
// usually should. Read this when the two need telling apart — an audit sink
// recording "restored" rather than "updated", an event emitter choosing a
// different type, or a validation rule that only makes sense against a body,
// since a restore carries none (ctx.ParsedBody is empty).
func (c *ServerContext) IsRestore() bool { return c.restore }

// executableOps lists the operations Execute can run: the five that go through
// the full six-step pipeline and produce a value rather than a byte stream.
var executableOps = map[Operation]string{
	OpCreate: http.MethodPost,
	OpRead:   http.MethodGet,
	OpUpdate: http.MethodPatch,
	OpDelete: http.MethodDelete,
	OpList:   http.MethodGet,
}

// Execute runs a registered model's operation through the full pipeline, from Go,
// and returns the response the same request over HTTP would have produced.
//
//	res, err := srv.Execute(ctx.Ctx, maniflex.Invocation{
//	    Model:     "Item",
//	    Operation: maniflex.OpUpdate,
//	    ID:        item.ResourceID,
//	    Body:      map[string]any{"status": "approved"},
//	    Auth:      &approver,  // a typed principal, not a header
//	    Tx:        ctx.Tx,     // joins the caller's transaction
//	})
//
// Every step runs — Auth, Deserialize, Validate, Service, DB, Response — in the
// order and with the middleware a client's request would get. There is no way to
// skip one; see the file comment for why.
//
// A non-2xx answer is returned as an *ExecuteError, so a loop of invocations
// inside one transaction rolls back on the first failure rather than committing
// the prefix. The *APIResponse is returned either way.
//
// Refused: OpExport and OpReadAttachment write bytes to a response writer rather
// than producing a value, and OpAction and OpSearch run trimmed pipelines of
// their own — call an action's handler directly.
//
// Execute builds the router on first use, exactly as Handler() does, so the
// registration window closes: Pipeline.Register and Server.Action must precede it.
func (c *Server) Execute(ctx context.Context, inv Invocation) (*APIResponse, error) {
	method, ok := executableOps[inv.Operation]
	if !ok {
		return nil, fmt.Errorf(
			"maniflex: execute: operation %q cannot be executed in process — %s",
			inv.Operation, executeOpHint(inv.Operation))
	}
	if inv.Model == "" {
		return nil, fmt.Errorf("maniflex: execute: Model is required")
	}

	// Building the router is what resolves many-to-many relations and freezes the
	// pipeline. Skipping it would run against a half-resolved registry and a
	// middleware set still open to change — and would then differ from what the
	// same call makes over HTTP, which is the one thing this must not do. It is
	// idempotent: an already-serving server just takes the lock and returns.
	c.Handler()

	meta, ok := c.registry.Get(inv.Model)
	if !ok {
		return nil, fmt.Errorf("maniflex: execute: model %q is not registered", inv.Model)
	}
	if inv.ID == "" && (inv.Operation == OpRead || inv.Operation == OpUpdate || inv.Operation == OpDelete) {
		return nil, fmt.Errorf(
			"maniflex: execute: ID is required for %s on %s", inv.Operation, meta.Name)
	}

	req, err := synthRequest(ctx, method, meta, inv)
	if err != nil {
		return nil, err
	}

	h := newHandlers(c.Pipeline, c.steps, &c.cfg)
	sctx, cleanup := h.buildContext(&captureWriter{}, req, meta, inv.Operation)
	defer cleanup()

	sctx.inProcess = true
	sctx.ResourceID = inv.ID
	sctx.Auth = inv.Auth
	sctx.Tx = inv.Tx

	if err := c.Pipeline.execute(sctx); err != nil {
		return nil, fmt.Errorf("maniflex: execute: %s %s: %w", inv.Operation, meta.Name, err)
	}

	res := sctx.Response
	if res == nil {
		// Every full-pipeline operation's Response step sets one. A nil here means a
		// middleware replaced the step and returned nothing, which would otherwise
		// surface as a nil-map panic in the caller's own code.
		return nil, fmt.Errorf(
			"maniflex: execute: %s %s produced no response — a Response-step middleware "+
				"registered AtPosition(Replace) must set ctx.Response",
			inv.Operation, meta.Name)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res, executeErrorFrom(res)
	}
	return res, nil
}

// executeErrorFrom wraps a non-2xx response as an error.
func executeErrorFrom(res *APIResponse) *ExecuteError {
	e := &ExecuteError{StatusCode: res.StatusCode, Response: res}
	if res.Error != nil {
		e.Code, e.Message = res.Error.Code, res.Error.Message
	}
	return e
}

// executeOpHint says why an operation is refused, since "cannot be executed" on
// its own leaves the reader to guess whether it is a gap or a decision.
func executeOpHint(op Operation) string {
	switch op {
	case OpExport, OpReadAttachment:
		return "it streams bytes to a response writer rather than producing a value, " +
			"so there is nothing for Execute to return"
	case OpAction:
		return "an action runs its own trimmed pipeline; call its handler directly"
	case OpSearch:
		return "search runs its own trimmed pipeline; call ctx.Search instead"
	case OpHead:
		return "OpHead is never set — a HEAD request runs as OpRead or OpList"
	}
	return "only OpCreate, OpRead, OpUpdate, OpDelete and OpList run the full pipeline"
}

// synthRequest builds the *http.Request the pipeline steps read.
//
// This is the load-bearing trick: rather than teaching six steps and every
// shipped and third-party middleware to cope with a nil Request — none of them do
// today, and one missed deref is a panic in someone's approval queue — Execute
// hands them the thing they already expect. The body binds through the real
// Deserialize step, ?filter= parses through the real ParseQueryParams, and a
// middleware reading ctx.Request.Method sees the method the operation means.
func synthRequest(ctx context.Context, method string, meta *ModelMeta, inv Invocation) (*http.Request, error) {
	body, err := marshalBody(inv.Body)
	if err != nil {
		return nil, err
	}

	u := &url.URL{Path: executePath(meta, inv)}
	if len(inv.Query) > 0 {
		u.RawQuery = inv.Query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("maniflex: execute: build request: %w", err)
	}
	req.ContentLength = int64(len(body))
	if inv.Header != nil {
		req.Header = inv.Header.Clone()
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// A chi route context so ctx.URLParam("id") answers for middleware, the same
	// way it does under the router. buildContext reads the id from here.
	rctx := chi.NewRouteContext()
	if inv.ID != "" {
		rctx.URLParams.Add("id", inv.ID)
	}
	reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)

	// A request id, so an invocation's log lines correlate the way a request's do
	// rather than appearing under an empty id.
	reqCtx = context.WithValue(reqCtx, chiMiddleware.RequestIDKey, "execute/"+uuid.NewString())

	return req.WithContext(reqCtx), nil
}

// executePath renders the path this invocation would have had over HTTP, so a
// middleware keying on ctx.Request.URL.Path (an audit log, a rate limiter) reads
// what it would read for the same request from a client.
func executePath(meta *ModelMeta, inv Invocation) string {
	base := "/" + strings.TrimPrefix(meta.TableName, TABLE_NAME_PREFIX)
	if inv.ID != "" {
		return base + "/" + inv.ID
	}
	return base
}

// marshalBody renders Invocation.Body as the JSON the Deserialize step reads.
// Bytes are passed through so a caller replaying a stored payload gets exactly
// the bytes they stored, rather than a re-encoding of a decoding of them.
func marshalBody(body any) ([]byte, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case []byte:
		return b, nil
	case json.RawMessage:
		return b, nil
	case string:
		return []byte(b), nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("maniflex: execute: marshal Body: %w", err)
	}
	return out, nil
}

// captureWriter stands in for the http.ResponseWriter the steps and middleware
// expect. Nothing is meant to reach it — Execute reads ctx.Response, and the
// framework's own write happens outside the pipeline — but the pipeline hands the
// writer to http.MaxBytesReader and middleware sets headers on it, so it must be
// a real one rather than nil.
//
// What is written is kept rather than discarded so that a middleware writing the
// response itself fails visibly, in Execute's own error, instead of silently
// dropping bytes down a hole.
type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *captureWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *captureWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

var _ http.ResponseWriter = (*captureWriter)(nil)
var _ io.Writer = (*captureWriter)(nil)
