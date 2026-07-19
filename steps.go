package maniflex

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxBodyBytes = 4 << 20 // 4 MB

// defaultSteps owns the built-in handler for every pipeline step.
type defaultSteps struct {
	adapter     DBAdapter
	reg         RegistryAccessor
	storage     FileStorage
	keyProvider KeyProvider
	// keyScope resolves the owning principal of a minted file key; nil →
	// defaultKeyScope (P1-20).
	keyScope     func(ctx *ServerContext) string
	signedURLTTL time.Duration // TTL for FileACLSigned URLs; 0 → DefaultFileSignedURLTTL
	// maxUpload caps a multipart request's total size; maxUploadMem caps how much
	// of it is buffered in memory. Zero → DefaultMaxUploadBytes / DefaultMaxUploadMemory.
	maxUpload    int64
	maxUploadMem int64
	bg           *backgroundRunner
}

func newDefaultSteps(adapter DBAdapter, reg RegistryAccessor) *defaultSteps {
	return &defaultSteps{adapter: adapter, reg: reg, bg: newBackgroundRunner()}
}

// ── Auth ──────────────────────────────────────────────────────────────────────
// Passthrough by default. Replace with your own auth middleware.

func (s *defaultSteps) auth(ctx *ServerContext, next func() error) error {
	// Auth dump: log resolved identity after all Before-Auth middleware have run.
	// ctx.Auth is nil when the request is unauthenticated (anonymous access).
	if ctx.trace != nil && ctx.trace.Steps {
		if ctx.Auth != nil {
			ctx.Logger().Debug("auth resolved",
				slog.String("user_id", ctx.Auth.UserID),
				slog.Any("roles", ctx.Auth.Roles),
			)
		} else {
			ctx.Logger().Debug("auth resolved", slog.String("user_id", "(anonymous)"))
		}
	}
	return next()
}

// ── Deserialize ───────────────────────────────────────────────────────────────
// Parses the JSON request body (for write operations) and URL query params.

func (s *defaultSteps) deserialize(ctx *ServerContext, next func() error) error {
	// Query params are needed for all operations (includes on reads, filters on lists)
	q, err := ParseQueryParams(ctx.Request, ctx.Model, s.reg)
	if err != nil {
		ctx.Abort(http.StatusBadRequest, "INVALID_QUERY", err.Error())
		return nil
	}
	enrichLocaleQueryParams(q, ctx, ctx.Model)

	// Carry over forced filters an earlier step set on ctx.Query. ParseQueryParams
	// builds a fresh QueryParams from the request, so a scope appended before this
	// step — an Auth-step middleware restricting a caller to their own rows — would
	// otherwise be silently discarded (this is what left jobs/maniflex's job-status
	// scope inert: P1-18). Only Forced filters are preserved: a scope the server
	// imposed, marked with FilterExpr.Forced, survives, while anything derived from
	// the request itself comes from the parse alone, so a client cannot smuggle a
	// filter in ahead of the parse.
	if ctx.Query != nil && q != nil {
		for _, f := range ctx.Query.Filters {
			if f != nil && f.Forced {
				q.Filters = append(q.Filters, f)
			}
		}
	}
	ctx.Query = q

	// Capture the ?select= projection set onto the transient carrier-staging
	// field (Phase 1; wired into the typed record's selectFn in Phase 4).
	if sel := selectKeysFromRequest(ctx.Request); sel != nil {
		ctx.selectKeys = sel
	}

	// Query params trace: log parsed pagination, filters, sorts, and includes.
	if ctx.trace != nil && ctx.trace.Steps && q != nil {
		attrs := []slog.Attr{
			slog.Int("page", q.Page),
			slog.Int("limit", q.Limit),
			slog.Int("filters", len(q.Filters)),
			slog.Int("sorts", len(q.Sorts)),
		}
		if len(q.Includes) > 0 {
			attrs = append(attrs, slog.Any("includes", q.Includes))
		}
		ctx.Logger().LogAttrs(context.Background(), slog.LevelDebug, "query params parsed", attrs...)
	}

	// Aggregate endpoint (4.7): ?aggregate= carries the spec as URL-encoded JSON.
	// It is a query parameter and not a body because this is a GET, and a GET body
	// is dropped by many proxies and CDNs and cannot be sent by fetch() at all —
	// an endpoint that needs one works in development and fails in production
	// (DX-2). Parse and validate here so an invalid query fails fast with a 400
	// before reaching the DB step. The standard create/update body handling below
	// does not run for this request.
	//
	// The parameter is "aggregate" rather than the shorter "q" because ?q= is the
	// full-text search parameter, and this request runs as a list.
	if ctx.aggregate {
		spec := strings.TrimSpace(ctx.QueryParam("aggregate"))
		if spec == "" {
			msg := "aggregate query must be supplied as URL-encoded JSON in the ?aggregate= parameter"
			if ctx.Request.ContentLength > 0 {
				msg += "; the request body is not read (a GET body does not survive most proxies)"
			}
			ctx.Abort(http.StatusBadRequest, "INVALID_AGGREGATE", msg)
			return nil
		}
		q, err := buildAggregateQuery([]byte(spec), ctx.Model)
		if err != nil {
			ctx.Abort(http.StatusBadRequest, "INVALID_AGGREGATE", err.Error())
			return nil
		}
		ctx.aggQuery = &q
		return next()
	}

	// A restore (5.19) dispatches as OpUpdate but carries no body — it names the
	// row in the URL and says only "undo the delete". Reading a body here would
	// reject every restore with EMPTY_BODY.
	if (ctx.Operation == OpCreate || ctx.Operation == OpUpdate) && !ctx.restore {
		contentType := ctx.Request.Header.Get("Content-Type")

		if strings.HasPrefix(contentType, "multipart/form-data") {
			if err := s.parseMultipart(ctx); err != nil {
				return nil // ctx.Abort already called inside parseMultipart
			}
		} else {
			body, err := ctx.readLimitedBody()
			if err != nil {
				return nil // ctx.Abort already called
			}
			if len(body) == 0 {
				ctx.Abort(http.StatusBadRequest, "EMPTY_BODY", "request body must not be empty")
				return nil
			}
			ctx.RawBody = body

			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err != nil {
				ctx.Abort(http.StatusBadRequest, "INVALID_JSON", fmt.Sprintf("malformed JSON: %s", err.Error()))
				return nil
			}
			if ctx.ParsedBody == nil {
				ctx.ParsedBody = NewRequestBody(nil)
			}
			maps.Copy(ctx.ParsedBody.m, parsed)

			// Capture top-level key presence (PATCH semantics) onto the transient
			// carrier-staging field. A separate RawMessage decode counts an
			// explicit {"x": null} as present while an omitted key is absent —
			// the distinction Phase-4 PATCH relies on. Additive: ParsedBody is
			// still populated exactly as before.
			if present, err := presentKeysFromJSON(body); err == nil {
				ctx.present = present
			}

			// Bind the body into the typed record carrier (ctx.Record) alongside
			// ParsedBody. Best-effort: ParsedBody remains the authoritative body
			// for the still-map validate/db steps during the transition, so a
			// decode mismatch here never fails the request.
			s.bindRecord(ctx, body)
		}

		// Body fields trace: log field names only (not values) to avoid leaking
		// sensitive data (passwords, tokens) into logs.
		if ctx.trace != nil && ctx.trace.Bodies && ctx.ParsedBody != nil {
			keys := ctx.ParsedBody.Keys()
			sort.Strings(keys)
			ctx.Logger().Debug("parsed body fields", slog.Any("fields", keys))
		}
	}

	return next()
}

// presentKeysFromJSON returns the set of top-level keys in a JSON object body.
// It decodes into map[string]json.RawMessage so an explicit null value counts
// as present (the key exists) — unlike a value-typed decode where null and
// absent both yield the zero value. A non-object body yields a nil set + error.
func presentKeysFromJSON(body []byte) (map[string]struct{}, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	keys := make(map[string]struct{}, len(raw))
	for k := range raw {
		keys[k] = struct{}{}
	}
	return keys, nil
}

// bindRecord decodes a JSON body into a typed record carrier (*T for
// ctx.Model.GoType) and stores it on ctx.Record, recording the present columns
// (DB names, translated from the body's JSON keys) and the ?select= set on the
// carrier. Best-effort: a decode error or a GoType-less model leaves ctx.Record
// nil and the still-map write path proceeds from ParsedBody.
func (s *defaultSteps) bindRecord(ctx *ServerContext, body []byte) {
	if ctx.Model == nil || ctx.Model.GoType == nil {
		return
	}
	rec := reflect.New(ctx.Model.GoType).Interface()
	if err := json.Unmarshal(body, rec); err != nil {
		return
	}
	rm, ok := rec.(recordMeta)
	if !ok {
		return
	}
	present := make(map[string]struct{}, len(ctx.present))
	for jsonName := range ctx.present {
		if f := ctx.Model.FieldByJSONName(jsonName); f != nil {
			present[f.Tags.DBName] = struct{}{}
		}
	}
	rm.mfxSetPresent(present)
	if ctx.selectKeys != nil {
		rm.mfxSetSelect(ctx.selectKeys)
	}
	ctx.Record = rec
}

// selectKeysFromRequest parses ?select=a,b,c into a JSON-name set, or nil when
// the parameter is absent. The set is staged on ctx for Phase-4 response
// projection; ParseQueryParams independently resolves the same list into DB
// columns for the SELECT.
func selectKeysFromRequest(r *http.Request) map[string]struct{} {
	if r == nil {
		return nil
	}
	sel := r.URL.Query().Get("select")
	if sel == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, name := range strings.Split(sel, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// uploadLimit is the ceiling on this request's total multipart body. A
// Deserialize-step body.MaxBodySize (ctx.SetMaxBodySize) wins when set, so a
// model can be held to a tighter bound than the app-wide default.
func (s *defaultSteps) uploadLimit(ctx *ServerContext) int64 {
	if ctx != nil && ctx.maxBody > 0 {
		return ctx.maxBody
	}
	return uploadLimitOr(s.maxUpload)
}

// uploadMemory is how much of a multipart body is buffered in memory before the
// rest spools to temp files.
func (s *defaultSteps) uploadMemory() int64 {
	return uploadMemoryOr(s.maxUploadMem)
}

// limitMultipartBody caps the request body at limit bytes. Reading past it fails
// the read, which is what keeps an unbounded upload from ever reaching disk.
func limitMultipartBody(ctx *ServerContext, limit int64) {
	if ctx.Request.Body != nil {
		ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, limit)
	}
}

// isBodyTooLarge reports whether err is the overflow from an http.MaxBytesReader.
// mime/multipart wraps the reader's error in its own, and older paths surface it
// as a bare string, so check both.
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr) ||
		(err != nil && strings.Contains(err.Error(), "request body too large"))
}

// parseMultipart handles multipart/form-data requests. Form values go into
// ctx.ParsedBody, file parts go into ctx.Files. File readers remain open
// until dispatch() cleans them up after the pipeline completes.
//
// A model with an mfx:"file,upload:stream" field takes the streaming parser,
// which pipes that field's bytes straight to storage as they arrive rather than
// spooling the whole request to disk first. Every other model — the common case
// — takes the unchanged buffered path below, so existing uploads are untouched.
func (s *defaultSteps) parseMultipart(ctx *ServerContext) error {
	if ctx.Model != nil && ctx.Model.hasStreamingFileField() {
		return s.parseMultipartStreaming(ctx)
	}
	return s.parseMultipartBuffered(ctx)
}

// parseMultipartBuffered is the default multipart parser: it reads the whole
// request via ParseMultipartForm (in-memory up to MaxUploadMemory, the rest to
// temp files) before the pipeline continues.
func (s *defaultSteps) parseMultipartBuffered(ctx *ServerContext) error {
	// Bound the request before parsing it. ParseMultipartForm's argument caps only
	// the in-memory buffer — everything beyond it spools to temp files, so without
	// a ceiling on the body itself a client can stream until the disk fills
	// (BUG-5). MaxBytesReader stops the read at the limit instead.
	limitMultipartBody(ctx, s.uploadLimit(ctx))

	if err := ctx.Request.ParseMultipartForm(s.uploadMemory()); err != nil {
		if isBodyTooLarge(err) {
			_ = ctx.abortBodyTooLarge(s.uploadLimit(ctx))
			return err
		}
		ctx.Abort(http.StatusBadRequest, "MULTIPART_ERROR",
			fmt.Sprintf("failed to parse multipart form: %s", err.Error()))
		return err
	}

	// Parse form values into ctx.ParsedBody
	if ctx.ParsedBody == nil {
		ctx.ParsedBody = NewRequestBody(nil)
	}
	present := make(map[string]struct{})
	for key, values := range ctx.Request.MultipartForm.Value {
		if len(values) > 0 {
			ctx.ParsedBody.m[key] = values[0]
		}
		present[key] = struct{}{}
	}

	// File parts are present write fields too (PATCH semantics, Phase 1).
	for key := range ctx.Request.MultipartForm.File {
		present[key] = struct{}{}
	}
	ctx.present = present

	// Parse file parts into ctx.Files
	files := make(map[string]*UploadedFile)
	for key, fileHeaders := range ctx.Request.MultipartForm.File {
		if len(fileHeaders) == 0 {
			continue
		}
		fh := fileHeaders[0] // single file per field
		f, err := fh.Open()
		if err != nil {
			ctx.Abort(http.StatusBadRequest, "FILE_READ_ERROR",
				fmt.Sprintf("failed to open uploaded file %q: %s", key, err.Error()))
			return err
		}
		contentType, err := resolveContentType(fh.Header.Get("Content-Type"), f)
		if err != nil {
			ctx.Abort(http.StatusInternalServerError, "FILE_READ_ERROR",
				fmt.Sprintf("failed to read uploaded file %q: %s", key, err.Error()))
			return err
		}

		files[key] = &UploadedFile{
			Filename:    fh.Filename,
			ContentType: contentType,
			Size:        fh.Size,
			Reader:      f,
		}
	}
	ctx.Files = files

	return nil
}

// ── Validate ──────────────────────────────────────────────────────────────────
// Enforces mfx struct-tag rules against ctx.ParsedBody.
// Runs only for create and update operations.

// fieldAcceptsNull reports whether a JSON null can be stored in this field.
//
// It mirrors the migrator's column rule exactly — a pointer field gets a NULL
// column, everything else gets NOT NULL (see buildColumnDef) — because the two
// must not drift: validation that accepted a null the schema then refused would
// hand the client the database's own constraint error, which is how audit MS-7
// presented in the first place.
//
// So the answer is the Go type alone, not the tags. A slice, map or
// sql.Scanner-implementing field is still NOT NULL when it is not a pointer, and
// a null for it is refused on the same terms.
func fieldAcceptsNull(f FieldMeta) bool {
	return f.Type != nil && f.Type.Kind() == reflect.Pointer
}

func (s *defaultSteps) validate(ctx *ServerContext, next func() error) error {
	if ctx.Operation != OpCreate && ctx.Operation != OpUpdate {
		return next()
	}

	// lock_when: on Update, refuse to proceed if the existing record's state
	// matches any locking condition. Create is exempt (there is no prior state)
	// but a record could be created already in a "locked" state — that's a
	// caller decision, not something we enforce here.
	if ctx.Operation == OpUpdate && len(ctx.Model.LockWhen) > 0 {
		if resp := s.checkRecordLocked(ctx); resp != nil {
			ctx.Response = resp
			return nil
		}
	}

	body := ctx.ParsedBody
	var errs []map[string]string

	for _, field := range ctx.Model.Fields {
		jn := field.Tags.JSONName

		// ID is always generated by the adapter; strip any client-supplied value.
		if field.Tags.DBName == "id" {
			ctx.DeleteField(jn)
			continue
		}

		// Readonly fields are never accepted from clients — but a value the
		// server stamped is not from a client. Without this a middleware running
		// before Validate (db.Tenancy hoisted with ProvidesScope, an auth
		// middleware setting an owner column) had its injection stripped, and the
		// row was written with the field empty: invisible to the tenant that
		// created it, and visible to anyone whose scope is also empty.
		if field.Tags.Readonly && !ctx.ServerSetField(jn) {
			ctx.DeleteField(jn)
			continue
		}

		// Immutable fields may be set on create but not changed on update.
		if field.Tags.Immutable && ctx.Operation == OpUpdate && !ctx.ServerSetField(jn) {
			ctx.DeleteField(jn)
			continue
		}

		// Required fields must be present and non-nil on create.
		// For file fields, the value may come from ctx.Files (multipart upload)
		// instead of ctx.ParsedBody (JSON body).
		if field.Tags.Required && ctx.Operation == OpCreate {
			val, presentInBody := body.Get(jn)
			hasFile := field.Tags.File && ctx.Files != nil && ctx.Files[jn] != nil
			if !presentInBody && !hasFile {
				errs = append(errs, map[string]string{
					"field":   jn,
					"message": fmt.Sprintf("field %q is required", jn),
				})
				continue
			}
			if presentInBody && val == nil && !hasFile {
				errs = append(errs, map[string]string{
					"field":   jn,
					"message": fmt.Sprintf("field %q is required", jn),
				})
				continue
			}
		}

		// An explicit JSON null is refused for a field whose Go type cannot hold
		// one (audit MS-7). It used to depend on which source the write was read
		// from: the record path took json.Unmarshal's collapse of null into the
		// Go zero value and stored "" with a 200, while the map path carried the
		// nil through to a NOT NULL column and surfaced the database's own
		// constraint violation as a 422. Same request, two answers, decided by
		// whether a middleware had left the body and the typed record's present
		// set disagreeing.
		//
		// Refusing here settles it before either path runs, and refusing is the
		// honest answer rather than a choice between them: the column is NOT NULL
		// precisely because the field is not a pointer, so there is no null for
		// it to store, and silently writing "" would answer a different request
		// from the one that was sent. Nullability is spelled *T.
		if val, present := body.Get(jn); present && val == nil && !fieldAcceptsNull(field) {
			errs = append(errs, map[string]string{
				"field": jn,
				"message": fmt.Sprintf(
					"field %q cannot be null; its type has no null value — "+
						"send a value, omit the field, or make it a pointer to allow null", jn),
			})
			continue
		}

		// Enum and numeric min/max, shared with the programmatic write paths so
		// the two cannot drift apart (see checkFieldValue, audit MS-15).
		if val, present := body.Get(jn); present && val != nil {
			if msg := checkFieldValue(&field, val); msg != "" {
				errs = append(errs, map[string]string{"field": jn, "message": msg})
			}
		}
	}

	if len(errs) > 0 {
		ctx.Response = &APIResponse{
			StatusCode: http.StatusUnprocessableEntity,
			Error: &APIError{
				Code:    "VALIDATION_ERROR",
				Message: "one or more fields failed validation",
				Details: errs,
			},
		}
		return nil
	}

	return next()
}

// ── Service ───────────────────────────────────────────────────────────────────
// Noop by default — register your business logic here.
// When the model has mfx:"file" fields and storage is configured, the default
// handler also processes file uploads (validate, store, set key in ParsedBody).

func (s *defaultSteps) service(ctx *ServerContext, next func() error) error {
	if (ctx.Operation == OpCreate || ctx.Operation == OpUpdate) &&
		s.storage != nil && ctx.Model.HasFileFields() {
		if err := s.processFileFields(ctx); err != nil {
			return nil // ctx.Abort already called
		}
	}
	return next()
}

// processFileFields handles file fields for create/update operations.
// For each file field on the model it handles four cases:
//   - Case A: file uploaded via multipart (entry in ctx.Files)
//   - Case B: string key provided (pre-uploaded file reference in ctx.ParsedBody)
//   - Case C: explicit null in the body — clear the field (and delete the
//     stored object when the field is mfx:"auto_delete")
//   - Case D: field absent — skip (optional field or PATCH that doesn't touch it)
//
// Case C is JSON-Merge-Patch semantics: `PATCH {"avatar": null}` removes the
// attachment. The previous behaviour treated null as "absent" so callers had
// no way to detach a file without editing the DB directly.
func (s *defaultSteps) processFileFields(ctx *ServerContext) error {
	for _, field := range ctx.Model.FileFields() {
		jn := field.Tags.JSONName

		// An upload:stream field was already stored during Deserialize; its key is
		// in the body and its object is tracked for cleanup. Re-processing it here
		// would Stat the key back as if it were a client-supplied reference.
		if ctx.isStreamedFile(jn) {
			continue
		}

		if uf, ok := ctx.Files[jn]; ok {
			// Case A: file uploaded. A key list cannot be populated this way —
			// multipart carries one file per field (fileHeaders[0]), so letting
			// this through would write a single key into a column that holds an
			// array, and silently drop every other part the client sent.
			if field.IsFileList() {
				ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
					fmt.Sprintf("field %q holds many files and cannot be uploaded as multipart — "+
						"upload each file (POST /files, or a presigned upload) and send the keys as an array", jn))
				return fmt.Errorf("field %q: multipart upload to a file list", jn)
			}
			if err := s.handleFileUpload(ctx, field, uf); err != nil {
				return err
			}
			continue
		}
		keyVal, present := ctx.ParsedBody.Get(jn)
		if !present {
			// Case D: field absent — nothing to do
			continue
		}
		if keyVal == nil {
			// Case C: explicit null — clear the field. For auto_delete fields
			// also queue the existing object for removal via the same
			// background runner the auto-delete cleanup uses, so the storage
			// call participates in Shutdown coupling and doesn't block the
			// HTTP response.
			s.handleFileClear(ctx, field)
			continue
		}
		// Case B: key reference — one key, or a FileKeys list.
		if field.Type == fileKeysType {
			if err := s.handleFileKeyList(ctx, field, keyVal); err != nil {
				return err
			}
			continue
		}
		keyStr, isString := keyVal.(string)
		if isString && keyStr != "" {
			if err := s.handleFileKeyReference(ctx, field, keyStr); err != nil {
				return err
			}
		}
	}
	return nil
}

// handleFileKeyList verifies every key of a maniflex.FileKeys field and handles
// GC of the keys this write drops.
//
// Each key runs the same checkStoredFileRules the single-key path runs, so
// max_size and accept bind per object rather than to the array as a whole — a
// gallery of ten 5MB images is ten valid files, not one 50MB one.
func (s *defaultSteps) handleFileKeyList(ctx *ServerContext, field FieldMeta, keyVal any) error {
	keys, err := s.parseFileKeyList(ctx, field, keyVal)
	if err != nil {
		return err
	}
	if err := s.verifyFileKeys(ctx, field, keys); err != nil {
		return err
	}
	s.gcDroppedFileKeys(ctx, field, keys)

	// Normalise to FileKeys so the write path hands the column a driver.Valuer.
	// The body decodes to []any, which database/sql cannot store.
	ctx.SetField(field.Tags.JSONName, FileKeys(keys))
	return nil
}

// parseFileKeyList coerces the body value to a key list and bounds its length.
func (s *defaultSteps) parseFileKeyList(ctx *ServerContext, field FieldMeta, keyVal any) ([]string, error) {
	jn := field.Tags.JSONName
	keys, ok := toFileKeys(keyVal)
	if !ok {
		ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			fmt.Sprintf("field %q takes an array of storage keys", jn))
		return nil, fmt.Errorf("field %q: not a key array", jn)
	}

	// Bound the array before touching storage: every key costs a Stat, so an
	// uncapped list is one request buying N round-trips.
	maxCount := field.Tags.MaxCount
	if maxCount <= 0 {
		maxCount = DefaultMaxFileCount
	}
	if len(keys) > maxCount {
		ctx.Abort(http.StatusUnprocessableEntity, "TOO_MANY_FILES",
			fmt.Sprintf("field %q accepts at most %d files, got %d", jn, maxCount, len(keys)))
		return nil, fmt.Errorf("field %q: too many files", jn)
	}
	return keys, nil
}

// verifyFileKeys Stats every key and runs the field's rules against each object.
func (s *defaultSteps) verifyFileKeys(ctx *ServerContext, field FieldMeta, keys []string) error {
	jn := field.Tags.JSONName
	for _, key := range keys {
		if key == "" {
			ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				fmt.Sprintf("field %q contains an empty storage key", jn))
			return fmt.Errorf("field %q: empty key", jn)
		}
		// Refuse a key minted for another principal, per key, before its Stat —
		// one leaked key in the array taints the whole write (P1-20).
		if err := s.verifyKeyScope(ctx, field, key); err != nil {
			return err
		}
		meta, err := s.storage.Stat(ctx.Ctx, key)
		if err != nil {
			if errors.Is(err, ErrFileNotFound) {
				ctx.Abort(http.StatusUnprocessableEntity, "FILE_NOT_FOUND",
					fmt.Sprintf("file key %q for field %q does not exist in storage", key, jn))
				return fmt.Errorf("file key not found")
			}
			ctx.Abort(http.StatusInternalServerError, "STORAGE_ERROR",
				fmt.Sprintf("failed to verify file key for %q: %s", jn, err.Error()))
			return err
		}
		if err := s.checkStoredFileRules(ctx, field, key, meta); err != nil {
			return err
		}
	}
	return nil
}

// gcDroppedFileKeys records for deletion every key the record held before this
// write and no longer holds after it.
//
// A PATCH replaces the whole array, so "dropped" is a set difference, not a
// positional one: reordering a gallery deletes nothing. Deletion happens after
// the write commits, via the same deleteReplacedFiles runner the single-key path
// uses — its replacedFiles list was already list-shaped.
func (s *defaultSteps) gcDroppedFileKeys(ctx *ServerContext, field FieldMeta, keys []string) {
	if ctx.Operation != OpUpdate || !field.Tags.AutoDelete {
		return
	}
	kept := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		kept[k] = struct{}{}
	}
	for _, old := range s.getOldFileKeys(ctx, field) {
		if _, still := kept[old]; !still && old != "" {
			ctx.trackReplacedFile(old, s.storage)
		}
	}
}

// getOldFileKeys returns the keys a FileKeys field holds in the record as it
// stands before this write.
func (s *defaultSteps) getOldFileKeys(ctx *ServerContext, field FieldMeta) []string {
	cols := s.existingColumns(ctx)
	if cols == nil {
		return nil
	}
	v, ok := cols[field.Tags.DBName]
	if !ok || v == nil {
		return nil
	}
	if keys, ok := toFileKeys(v); ok {
		return keys
	}
	// A map-shaped read carries the column as its stored JSON text.
	if raw, ok := v.(string); ok {
		var out FileKeys
		if err := out.unmarshal([]byte(raw)); err == nil {
			return out
		}
	}
	return nil
}

// toFileKeys coerces a file-list value to []string. The JSON body decodes to
// []any of strings; a typed record carries FileKeys directly.
func toFileKeys(v any) ([]string, bool) {
	switch t := v.(type) {
	case FileKeys:
		return t, true
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// handleFileClear processes an explicit null on a file field. The DB write
// will set the column to NULL; when the field is mfx:"auto_delete" the existing
// storage object is recorded for removal after the request succeeds (deleted by
// deleteReplacedFiles, not before the DB write — so a failed clear never orphans
// the row by deleting a blob it still references, BUG-1).
func (s *defaultSteps) handleFileClear(ctx *ServerContext, field FieldMeta) {
	if field.Tags.AutoDelete && ctx.Operation == OpUpdate {
		if oldKey, ok := s.getOldFileKey(ctx, field); ok && oldKey != "" {
			ctx.trackReplacedFile(oldKey, s.storage)
		}
	}
	// Leave ctx.ParsedBody[jn] = nil so the adapter writes NULL into the
	// column. Callers that want to also clear other side-effect columns
	// (e.g. {field}_hmac) handle that via their own Validate middleware.
}

// checkFileRules enforces an mfx:"file" field's max_size and accept rules
// against one candidate's size and type.
//
// Both ways a file can reach a field run through this, and that is the point:
// the bytes are the same bytes whether they arrived as multipart through the
// server or were uploaded straight to storage and named by key afterwards, so
// the answer must be the same too. Keeping one copy of the rule is what stops
// the two paths drifting into disagreeing about it, which is exactly what had
// happened — the key path enforced neither.
func (s *defaultSteps) checkFileRules(ctx *ServerContext, field FieldMeta,
	size int64, contentType string,
) error {
	jn := field.Tags.JSONName

	if field.Tags.MaxSize > 0 && size > field.Tags.MaxSize {
		ctx.Abort(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
			fmt.Sprintf("file %q exceeds maximum size of %d bytes", jn, field.Tags.MaxSize))
		return fmt.Errorf("file too large")
	}
	if !matchesAccept(contentType, field.Tags.Accept) {
		ctx.Abort(http.StatusUnsupportedMediaType, "FILE_TYPE_NOT_ALLOWED",
			fmt.Sprintf("file %q has type %q, allowed: %v", jn, contentType, field.Tags.Accept))
		return fmt.Errorf("file type not allowed")
	}
	return nil
}

// checkStoredFileRules applies checkFileRules to an object already in storage,
// named by key.
//
// A backend that reports no content type at all is the awkward case: refusing
// would break every LocalStorage deployment whose metadata sidecar predates
// this, while accepting means an accept rule passes on an unknown type. It is
// refused when the field constrains the type and accepted when it does not, so
// the rule is never quietly waived — an accept list that cannot be checked is
// not satisfied.
func (s *defaultSteps) checkStoredFileRules(ctx *ServerContext, field FieldMeta,
	key string, meta FileMeta,
) error {
	if meta.ContentType == "" && len(field.Tags.Accept) > 0 {
		ctx.Abort(http.StatusUnsupportedMediaType, "FILE_TYPE_NOT_ALLOWED",
			fmt.Sprintf("file key %q for field %q has no content type recorded in storage, "+
				"so it cannot be checked against the field's accepted types %v",
				key, field.Tags.JSONName, field.Tags.Accept))
		return fmt.Errorf("file type unknown")
	}
	return s.checkFileRules(ctx, field, meta.Size, meta.ContentType)
}

// handleFileUpload validates and stores an uploaded file, then sets the storage
// key in ctx.ParsedBody so it flows through to the DB as a normal string value.
func (s *defaultSteps) handleFileUpload(ctx *ServerContext, field FieldMeta, uf *UploadedFile) error {
	jn := field.Tags.JSONName

	if err := s.checkFileRules(ctx, field, uf.Size, uf.ContentType); err != nil {
		return err
	}

	// On update with auto_delete: record the old file for deletion after the
	// request succeeds, rather than deleting it now — a later failure (DB
	// constraint, post-Service abort) would otherwise orphan the row (BUG-1).
	if ctx.Operation == OpUpdate && field.Tags.AutoDelete {
		if oldKey, ok := s.getOldFileKey(ctx, field); ok && oldKey != "" {
			ctx.trackReplacedFile(oldKey, s.storage)
		}
	}

	// Generate storage key: {table}/{record_uuid}/{field_db_name}/{sanitized_filename}
	// then bind it to the uploading principal (P1-20), so if the key is later
	// referenced by name it is refused for anyone but its owner.
	key := applyKeyScope(resolveKeyScope(s.keyScope, ctx), fmt.Sprintf("%s/%s/%s/%s",
		ctx.Model.TableName,
		uuid.New().String(),
		field.Tags.DBName,
		sanitizeFilename(uf.Filename)))

	meta := FileMeta{
		Key:         key,
		ContentType: uf.ContentType,
		Size:        uf.Size,
		Filename:    uf.Filename,
	}
	if err := s.storage.Store(ctx.Ctx, key, uf.Reader, meta); err != nil {
		ctx.Abort(http.StatusInternalServerError, "FILE_STORE_ERROR",
			fmt.Sprintf("failed to store file %q: %s", jn, err.Error()))
		return err
	}

	// Record the stored object so it is deleted if a later step (DB, or a
	// post-Service Validate/Auth middleware) fails the request before the row
	// is committed — otherwise the blob is orphaned (3B.2b).
	ctx.TrackStoredFile(key, s.storage)

	// Set the storage key so it gets written to DB. SetField writes through to
	// both ParsedBody and the typed record carrier (W2).
	ctx.SetField(jn, key)
	return nil
}

// handleFileKeyReference verifies a pre-uploaded file key exists in storage,
// and handles old-file cleanup on update.
func (s *defaultSteps) handleFileKeyReference(ctx *ServerContext, field FieldMeta, key string) error {
	jn := field.Tags.JSONName

	// Refuse a key minted for another principal before touching storage — a
	// leaked key must not be referenceable onto this caller's record (P1-20).
	if err := s.verifyKeyScope(ctx, field, key); err != nil {
		return err
	}

	// Read the object's metadata. This both proves the key exists and yields the
	// size and type the field's rules are checked against below — which is why it
	// is Stat and not Exists: a yes/no answer cannot enforce max_size, and
	// Retrieve would download the object to find out, which for the large files
	// this path exists for is precisely the cost being avoided.
	meta, err := s.storage.Stat(ctx.Ctx, key)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			ctx.Abort(http.StatusUnprocessableEntity, "FILE_NOT_FOUND",
				fmt.Sprintf("file key %q for field %q does not exist in storage", key, jn))
			return fmt.Errorf("file key not found")
		}
		ctx.Abort(http.StatusInternalServerError, "STORAGE_ERROR",
			fmt.Sprintf("failed to verify file key for %q: %s", jn, err.Error()))
		return err
	}

	// The field's rules bind here exactly as they bind an upload through the
	// server. They did not until v0.2.3: this path checked only that the key
	// existed, so uploading out of band and then referencing the key was a way
	// past max_size and accept — the same bytes the multipart path answered 413
	// for were accepted with a 201. A presigned upload makes this path the normal
	// one for the largest files in the system, which is exactly where an
	// unenforced size limit is worth the most.
	if err := s.checkStoredFileRules(ctx, field, key, meta); err != nil {
		return err
	}

	// On update with auto_delete: if the key changed, record the old file for
	// deletion after the request succeeds (not before the DB write — BUG-1).
	if ctx.Operation == OpUpdate && field.Tags.AutoDelete {
		if oldKey, ok := s.getOldFileKey(ctx, field); ok && oldKey != "" && oldKey != key {
			ctx.trackReplacedFile(oldKey, s.storage)
		}
	}

	// Leave ctx.ParsedBody[jn] as-is — the verified key flows through to DB
	return nil
}

// verifyKeyScope refuses a scoped file key that was minted for a different
// principal than the one referencing it now (P1-20). A key carrying no scope
// marker — one minted before this release, or for an anonymous caller — is left
// to the existence and rules checks, so the guarantee closes the cross-principal
// hole for every key minted under a principal without breaking references to
// keys that predate it. The check is I/O-free and runs before the Stat, so a
// cross-principal reference is refused without a storage round-trip.
func (s *defaultSteps) verifyKeyScope(ctx *ServerContext, field FieldMeta, key string) error {
	seg, scoped := keyScopeSegmentOf(key)
	if !scoped {
		return nil
	}
	caller := resolveKeyScope(s.keyScope, ctx)
	if caller != "" && scopeSegment(caller) == seg {
		return nil
	}
	ctx.Abort(http.StatusForbidden, "FILE_FORBIDDEN",
		fmt.Sprintf("file key for field %q was uploaded under a different principal "+
			"and cannot be referenced here", field.Tags.JSONName))
	return fmt.Errorf("file key scope mismatch for field %q", field.Tags.JSONName)
}

// checkRecordLocked loads the currently-stored record and tests every
// LockCondition on the model. Returns a 422 RECORD_LOCKED APIResponse when a
// condition matches, or nil to proceed.
//
// The read joins the request's transaction when one is active, so the guard sees
// the state the write is actually about to modify rather than a pre-transaction
// snapshot of it. Only a missing record passes the guard — the downstream step
// turns that into its normal 404. Every other adapter failure fails the request:
// a guard that swings open on a transient DB error is no guard at all (BUG-3).
func (s *defaultSteps) checkRecordLocked(ctx *ServerContext) *APIResponse {
	if ctx.ResourceID == "" {
		return nil
	}
	adapter := ctx.Model.ResolveAdapter(s.adapter)
	if adapter == nil {
		return nil
	}
	exec := dbExec{adapter: adapter, tx: ctx.Tx}
	existing, err := exec.FindByID(ctx.Ctx, ctx.Model,
		ctx.ResourceID, &QueryParams{Limit: 1, Page: 1})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // nothing to lock — the downstream step returns its 404
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			if clientGone(ctx) {
				return clientGoneResponse()
			}
			return &APIResponse{
				StatusCode: http.StatusGatewayTimeout,
				Error: &APIError{
					Code:    "TIMEOUT",
					Message: "request exceeded the configured query timeout",
				},
			}
		}
		return &APIResponse{
			StatusCode: http.StatusInternalServerError,
			Error: &APIError{
				Code:    "DB_ERROR",
				Message: fmt.Sprintf("could not verify record lock state: %v", err),
			},
		}
	}
	record := s.toJSONMap(existing, ctx.Model, ctx)
	for _, cond := range ctx.Model.LockWhen {
		if cond.matchesRecord(record) {
			return &APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &APIError{
					Code: "RECORD_LOCKED",
					Message: fmt.Sprintf(
						"record is locked: %s=%q", cond.JSONName, cond.Value),
				},
			}
		}
	}
	return nil
}

// enforceOptimisticLock runs the If-Match precondition for models that opt into
// ModelConfig.OptimisticLock. The check and the write it guards must be atomic —
// otherwise two clients holding the same ETag both pass the check and the second
// silently clobbers the first, which is the lost update the feature exists to
// prevent (BUG-2). So the check re-reads the row under a pessimistic lock inside
// a transaction: the request's own when one is active (WithTransaction), or one
// opened here for the duration of the DB step.
//
// It returns the exec the rest of the DB step must use (routed through the
// transaction) and the transaction this step owns — nil when it joined an
// existing one, in which case the caller must neither commit nor roll it back.
// A 412 or 404 is set on ctx.Response; the error return is for adapter failures.
func (s *defaultSteps) enforceOptimisticLock(ctx *ServerContext, exec dbExec, model *ModelMeta) (dbExec, Tx, error) {
	if !model.Config.OptimisticLock || ctx.Request == nil ||
		(ctx.Operation != OpUpdate && ctx.Operation != OpDelete) {
		return exec, nil, nil
	}
	// A restore has no If-Match semantics to enforce: the row it targets is
	// soft-deleted and so invisible to the version read below, and there is no
	// submitted state that could be stale — the request carries no fields.
	if ctx.restore {
		return exec, nil, nil
	}
	ifMatch := ctx.Request.Header.Get("If-Match")
	if ifMatch == "" {
		return exec, nil, nil
	}

	var own Tx
	if ctx.Tx == nil {
		// beginTx, not BeginTx: this is the framework's own transaction on the
		// CRUD path, and the public one refuses under an ActionScope. A CRUD
		// request never carries one today, but a guard that depends on that
		// staying true is a trap for whoever changes it.
		tx, err := ctx.beginTx(ctx.Ctx, nil)
		if err != nil {
			return exec, nil, err
		}
		own = tx
		ctx.Tx = tx
		exec = dbExec{adapter: exec.adapter, tx: tx}
	}
	return exec, own, s.checkOptimisticLock(ctx, exec, model, ifMatch)
}

// ensureScopeTx puts the request on a transaction, so that a scope check and the
// write it authorises are one unit and the row cannot leave scope in between —
// the same bargain enforceOptimisticLock strikes for If-Match.
//
// It joins the request's own transaction when it has one, in which case the
// returned Tx is nil and the caller must neither commit nor roll it back.
// Otherwise it opens one and hands the caller ownership of it.
func (s *defaultSteps) ensureScopeTx(ctx *ServerContext, exec dbExec) (dbExec, Tx, error) {
	if ctx.Tx != nil {
		return exec, nil, nil
	}
	// beginTx, not BeginTx: this is the framework's own transaction on the CRUD
	// path, and the public one wraps what it returns under an ActionScope. A CRUD
	// request never carries a scope today, but a guard that depends on that
	// staying true is a trap for whoever changes it.
	tx, err := ctx.beginTx(ctx.Ctx, nil)
	if err != nil {
		return exec, nil, err
	}
	ctx.Tx = tx
	return dbExec{adapter: exec.adapter, tx: tx}, tx, nil
}

// enforceWriteScope refuses an update or delete of a record that the request's
// server-imposed filters exclude.
//
// The adapter's write contract carries no filter — Update(ctx, model, id, record,
// present) and Delete(ctx, model, id) against FindByID(ctx, model, id, q) — so a
// FilterExpr that db.Tenancy or db.ForceFilter appended to ctx.Query.Filters had
// no reader on the write path: the emitted SQL was DELETE FROM t WHERE id = ?.
// Reads were scoped and writes were not, so any caller could update or delete a
// row their own reads 404 on, given its id. With Tenancy that was worse than a
// leak, because Tenancy also stamps the tenant column onto an update: the write
// handed the row to the caller and the owner lost it.
//
// So the record is read back through the same filters first, and a miss is the
// 404 the caller would have got had they tried to read it. Only filters marked
// Forced count: a client's own ?filter= reaches Query.Filters on a PATCH too, and
// it has never constrained a write — a stray query parameter turning a PATCH into
// a 404 would be a surprise, and it is not what needed fixing.
//
// The read costs nothing when nothing is scoped, which is the common case: with
// no forced filters this returns before touching the database.
//
// It returns the exec the rest of the DB step must use and the transaction this
// step owns — nil when it joined an existing one, in which case the caller must
// neither commit nor roll it back.
func (s *defaultSteps) enforceWriteScope(ctx *ServerContext, exec dbExec, model *ModelMeta) (dbExec, Tx, error) {
	if ctx.Operation != OpUpdate && ctx.Operation != OpDelete {
		return exec, nil, nil
	}
	// A restore's target is soft-deleted, so the read-back below cannot see it —
	// every read path applies the soft-delete condition — and would turn every
	// in-scope restore into a 404. The scope is not skipped: it is pushed into
	// the restore statement itself, which applies the same forced filters in its
	// WHERE. See Restorer.
	if ctx.restore {
		return exec, nil, nil
	}
	if ctx.Query == nil {
		return exec, nil, nil
	}
	forced := forcedFilters(ctx.Query.Filters)
	if len(forced) == 0 {
		return exec, nil, nil // nothing imposed — no read, no transaction
	}

	exec, own, err := s.ensureScopeTx(ctx, exec)
	if err != nil {
		return exec, own, err
	}

	// Page/Limit are set because a zero Limit means "no rows" to the adapter;
	// Filters are the forced ones alone, deliberately not ctx.Query's includes,
	// sorts or projection — this read exists only to answer "is this row in
	// scope?".
	scoped := &QueryParams{Page: 1, Limit: 1, Filters: forced}
	if _, err := exec.FindByID(ctx.Ctx, model, ctx.ResourceID, scoped); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Indistinguishable from a genuinely absent record, on purpose: a
			// caller learning that a row exists but belongs to someone else is
			// the same disclosure the scoped read already refuses them.
			ctx.Abort(http.StatusNotFound, "NOT_FOUND",
				fmt.Sprintf("%s with id %q not found", model.Name, ctx.ResourceID))
			return exec, own, nil
		}
		return exec, own, err
	}
	return exec, own, nil
}

// enforceParentScope refuses a write whose parent foreign key points outside the
// request's scope.
//
// A nested forced filter — what db.ForceFilterVia builds — scopes a model that
// carries no column to scope by, through the column its BelongsTo parent does:
// "a DamagedItem is yours if its Item is yours". The read path joins the parent
// and applies the predicate, and enforceWriteScope reads the row back through
// those same filters, so an update or delete of another tenant's child is
// already the 404 it should be.
//
// Neither of them looks at the foreign key itself, and the foreign key is what
// the whole scope hangs from — while being the one part of it the client
// supplies. On a create there is no row to read back, so nothing examines item_id
// at all and POST {"item_id": "<another tenant's>"} lands a row under their
// parent. On an update the row read back is the one the *old* item_id points at,
// so a PATCH that rewrites item_id passes a check of where the row used to be and
// then moves it somewhere else. Either way the child ends up under a parent the
// caller cannot see, and since the child's scope is read through that parent, so
// does the child: the owner cannot see it, and the caller who planted it can.
//
// So the parent the write names is read through the scope's own predicate, and a
// miss is the same 404 a scoped read of that parent would give. Only a scope that
// runs through a parent pays for it: a flat forced filter names a column on the
// row itself, which is checked where it is written, not through a join.
func (s *defaultSteps) enforceParentScope(ctx *ServerContext, exec dbExec, model *ModelMeta) (dbExec, Tx, error) {
	if ctx.Operation != OpCreate && ctx.Operation != OpUpdate {
		return exec, nil, nil
	}
	if ctx.Query == nil {
		return exec, nil, nil
	}
	nested := nestedForcedFilters(ctx.Query.Filters)
	if len(nested) == 0 {
		return exec, nil, nil // nothing scoped through a parent — no read, no transaction
	}

	var own Tx
	for _, f := range nested {
		var err error
		exec, own, err = s.checkParentInScope(ctx, exec, model, f, own)
		if err != nil || ctx.Response != nil {
			return exec, own, err
		}
	}
	return exec, own, nil
}

// checkParentInScope enforces one nested forced filter against the parent this
// write names. own carries the transaction an earlier filter in the same sweep
// opened, so several scopes on one model share one.
func (s *defaultSteps) checkParentInScope(ctx *ServerContext, exec dbExec, model *ModelMeta,
	f *FilterExpr, own Tx,
) (dbExec, Tx, error) {
	parent, ok := s.reg.Get(f.RelationModel)
	if !ok {
		// Fail closed. The filter names a parent the registry does not have, so the
		// scope cannot be checked — and an unenforceable scope must stop the
		// request, not be assumed satisfied.
		return exec, own, fmt.Errorf(
			"maniflex: the scope on %s.%s targets model %q, which is not registered",
			model.Name, f.RelationKey, f.RelationModel)
	}

	fkID, present := s.writtenFK(ctx, model, f.RelationFK)
	switch {
	case !present && ctx.Operation == OpUpdate:
		// The update leaves the foreign key alone, so it cannot move the row out of
		// scope, and enforceWriteScope has already read the row itself back through
		// this same filter.
		return exec, own, nil
	case fkID == "":
		// A child with no parent satisfies no scope that lives on the parent: the
		// join finds nothing, so the row would be invisible to the very caller who
		// created it. Refusing is the honest answer; writing it is not.
		ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR", fmt.Sprintf(
			"%s.%s is required: it is what scopes this record, which carries no scope of its own",
			model.Name, f.RelationFK))
		return exec, own, nil
	}

	if own == nil {
		var err error
		if exec, own, err = s.ensureScopeTx(ctx, exec); err != nil {
			return exec, own, err
		}
	}

	// A flat filter on the parent's own table now — the join is what made the
	// column reachable from the child, and we are reading the parent directly.
	scoped := &QueryParams{Page: 1, Limit: 1, Filters: []*FilterExpr{{
		Field: f.NestedField, Operator: f.Operator, Value: f.Value,
	}}}
	if _, err := exec.FindByID(ctx.Ctx, parent, fkID, scoped); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The parent's own 404, verbatim: a caller who cannot read that parent
			// learns exactly what a read of it would have told them, and no more.
			ctx.Abort(http.StatusNotFound, "NOT_FOUND",
				fmt.Sprintf("%s with id %q not found", parent.Name, fkID))
			return exec, own, nil
		}
		return exec, own, err
	}
	return exec, own, nil
}

// writtenFK returns the foreign-key value this write will store in fkColumn, and
// whether the write sets it at all. A PATCH that does not mention the column
// reports false — the distinction matters, because "not written" leaves the row
// where it is while "written empty" moves it out of every scope.
func (s *defaultSteps) writtenFK(ctx *ServerContext, model *ModelMeta, fkColumn string) (string, bool) {
	fk := model.FieldByDBName(fkColumn)
	if fk == nil {
		return "", false
	}
	v, ok := ctx.Field(fk.Tags.JSONName)
	if !ok || v == nil {
		return "", false
	}
	return strings.TrimSpace(fmt.Sprint(v)), true
}

// checkOptimisticLock re-reads the current record under a row lock, computes its
// ETag (MD5 of the JSON-mapped body — same algorithm as response.Cache), and
// sets a 412 or 404 response on ctx when the client's If-Match value does not
// match. The wildcard "*" matches any existing record. The lock is held until the
// enclosing transaction ends, so a concurrent writer cannot slip a write in
// between this check and the one that follows.
// Returns a non-nil error only for unexpected adapter failures.
func (s *defaultSteps) checkOptimisticLock(ctx *ServerContext, exec dbExec, model *ModelMeta, ifMatch string) error {
	current, err := exec.findByIDForUpdate(ctx.Ctx, model, ctx.ResourceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			ctx.Abort(http.StatusNotFound, "NOT_FOUND",
				fmt.Sprintf("%s with id %q not found", model.Name, ctx.ResourceID))
			return nil
		}
		return err
	}
	// RFC 9110 §13.1.1: "If-Match: *" stands for any current representation, so
	// the precondition holds whenever the resource exists — there is no validator
	// to compare against. Clients send it to mean "overwrite whatever is there,
	// but do not create it". Comparing the literal "*" to the ETag failed every
	// such request with a 412 on a resource that plainly existed. The row is
	// locked either way, so the write it guards stays atomic.
	if strings.TrimSpace(ifMatch) == "*" {
		return nil
	}

	jsonRow := s.toJSONMap(current, model, ctx)
	b, _ := json.Marshal(jsonRow)
	currentETag := fmt.Sprintf(`"%x"`, md5.Sum(b))
	if currentETag != ifMatch {
		ctx.Response = &APIResponse{
			StatusCode: http.StatusPreconditionFailed,
			Error: &APIError{
				Code:    "PRECONDITION_FAILED",
				Message: "resource has been modified since last read; fetch the latest ETag and retry",
			},
		}
	}
	return nil
}

// existingColumns returns the record as it stands before this request's write, as
// a DB-column map, reading it at most once per request. nil when there is nothing
// to read (no id, no adapter) or the read failed — callers treat both as "no
// previous value", which is what the per-call read did on error too.
//
// The row cannot move underneath the memo: every caller runs before this request's
// own write, and the value they want is precisely the pre-write one.
func (s *defaultSteps) existingColumns(ctx *ServerContext) map[string]any {
	ctx.existingOnce.Do(func() {
		adapter := ctx.Model.ResolveAdapter(s.adapter)
		if ctx.ResourceID == "" || adapter == nil {
			return
		}
		existing, err := adapter.FindByID(ctx.Ctx, ctx.Model, ctx.ResourceID,
			&QueryParams{Limit: 1, Page: 1})
		if err != nil {
			return
		}
		ctx.existingCols = recordToMap(ctx.Model, existing)
	})
	return ctx.existingCols
}

// getOldFileKey returns the current stored key for a file field, from the record as
// it stands before this write. It used to issue its own FindByID plus recordToMap
// on every call, and the file step calls it once per file field — so a model with
// three auto_delete fields read the same row three times to pull three values out
// of it (PERF-4).
func (s *defaultSteps) getOldFileKey(ctx *ServerContext, field FieldMeta) (string, bool) {
	cols := s.existingColumns(ctx)
	if cols == nil {
		return "", false
	}
	oldKey, ok := cols[field.Tags.DBName].(string)
	return oldKey, ok
}

// ensureSingletonRow resolves the row a Singleton model's GET/PATCH addresses,
// provisioning it on first access. It runs through exec, so a registered
// transaction sees its own write.
//
// Which row that is depends on whether the request is scoped.
//
// With no forced filter it is the one global row, under the well-known
// SingletonID — the "admin edits one config record" shape, and what a Singleton
// has always been.
//
// With one (db.Tenancy, db.ForceFilter) it is *the caller's* row: a Singleton is
// then one row per scope, which is what a per-tenant settings/profile/storefront
// record is. The scope is taken from the request's forced filters rather than
// from a config field of its own, because those filters already carry the one
// thing a bare column name cannot — where the value comes from. db.Tenancy's
// TenantFunc has answered that question since long before this, and asking it
// twice, in two places that can disagree, would be the worse API.
//
// This also retires a bug rather than working around it: db.Tenancy on a
// Singleton used to 404 forever, because provisioning inserted a filterless row
// with no tenant column while the read applied the filter. There was no working
// behaviour here to preserve — the combination simply did not work — which is
// what makes deriving the scope safe.
func (s *defaultSteps) ensureSingletonRow(ctx *ServerContext, exec dbExec, model *ModelMeta) error {
	var forced []*FilterExpr
	if ctx.Query != nil {
		forced = forcedFilters(ctx.Query.Filters)
	}
	if len(forced) == 0 {
		return s.ensureGlobalSingletonRow(ctx, exec, model)
	}
	return s.ensureScopedSingletonRow(ctx, exec, model, forced)
}

// ensureGlobalSingletonRow guarantees the one row under SingletonID exists.
//
// A concurrent first access can have two requests both miss the row and both
// insert it; the loser hits a primary-key constraint, which we treat as success
// because the row now exists either way.
func (s *defaultSteps) ensureGlobalSingletonRow(ctx *ServerContext, exec dbExec, model *ModelMeta) error {
	if _, err := exec.FindByID(ctx.Ctx, model, SingletonID, &QueryParams{Limit: 1, Page: 1}); err == nil {
		return nil // already provisioned
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	// Insert with only the fixed id; every other column takes its DB default
	// (singleton models reject mfx:"required" fields, so a default always exists).
	if _, err := exec.Create(ctx.Ctx, model, map[string]any{"id": SingletonID}); err != nil {
		var ce *ErrConstraint
		if errors.As(err, &ce) {
			return nil // raced with a concurrent provisioning — the row exists
		}
		return err
	}
	return nil
}

// ensureScopedSingletonRow resolves this scope's row and points ctx.ResourceID at
// it, provisioning it on first access.
//
// The row keeps an ordinary generated primary key: SingletonID is not used, and
// could not be — it is one value, and there is now one row per scope. Nothing
// downstream notices, because everything downstream addresses the record by
// ctx.ResourceID, which the handler had pinned to the placeholder and this
// replaces with the real id.
func (s *defaultSteps) ensureScopedSingletonRow(ctx *ServerContext, exec dbExec,
	model *ModelMeta, forced []*FilterExpr,
) error {
	cols, bad := scopeColumns(forced)
	if bad != nil {
		// Fail closed. A scope we cannot write is one whose row we cannot
		// provision, and provisioning it anyway would create a row the caller who
		// asked for it cannot then read.
		return fmt.Errorf(
			"maniflex: singleton %s is scoped by a filter on %q that is not a plain equality, "+
				"so there is no value to provision its row with — a scoped singleton needs a "+
				"scope it can write as well as read (db.Tenancy, db.ForceFilter), not one that "+
				"only narrows a query",
			model.Name, bad.Field)
	}

	id, err := s.findScopedSingleton(ctx, exec, model, forced)
	if err != nil {
		return err
	}
	if id != "" {
		ctx.ResourceID = id
		return nil
	}

	rec, err := exec.Create(ctx.Ctx, model, cols)
	if err != nil {
		var ce *ErrConstraint
		if !errors.As(err, &ce) {
			return err
		}
		// Raced with a concurrent first access. A unique index on the scope column
		// is what turns that race into this branch rather than into two rows — see
		// the Singleton docs; without one, the loser has nothing to collide with.
		id, ferr := s.findScopedSingleton(ctx, exec, model, forced)
		if ferr != nil {
			return ferr
		}
		if id == "" {
			return err // the constraint was something else entirely
		}
		ctx.ResourceID = id
		return nil
	}

	if id = singletonRecordID(model, rec); id == "" {
		return fmt.Errorf(
			"maniflex: singleton %s: the provisioned row came back with no id", model.Name)
	}
	ctx.ResourceID = id
	return nil
}

// findScopedSingleton returns the id of this scope's row, or "" when it has none
// yet. Limit 1 because there is meant to be exactly one; if a race without a
// unique index left two, the older wins and stays the caller's singleton.
func (s *defaultSteps) findScopedSingleton(ctx *ServerContext, exec dbExec,
	model *ModelMeta, forced []*FilterExpr,
) (string, error) {
	return findScopedSingletonID(ctx.Ctx, exec, model, forced)
}

// findScopedSingletonID reads the id of the row this scope's singleton lives in,
// returning "" when it has not been provisioned yet. Read-only: provisioning is
// ensureScopedSingletonRow's job, and ServerContext.ResolveResourceID shares
// this lookup precisely because it must not acquire that side effect.
func findScopedSingletonID(c context.Context, exec dbExec,
	model *ModelMeta, forced []*FilterExpr,
) (string, error) {
	rows, _, err := exec.FindMany(c, model, &QueryParams{Page: 1, Limit: 1, Filters: forced})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return singletonRecordID(model, rows[0]), nil
}

// ResolveResourceID returns the id of the record this request addresses.
//
// Almost always that is ctx.ResourceID, read straight from the URL. The
// exception is a Singleton, which has no {id} to read: the handler pins
// ResourceID to the SingletonID placeholder, and for a *scoped* singleton the DB
// step later swaps in the caller's real row id, since one fixed value cannot
// name one row per scope. Middleware at the DB pipeline's Before position runs
// ahead of that swap and therefore sees the placeholder — which addresses no
// row, so a read through it silently returns nothing (audit 13.9).
//
// Returns "" when a scoped singleton's row does not exist yet. That is the
// honest answer — there is no record to address — and callers should treat it
// the same way they treat a create, which is what the request is about to
// become. Never provisions the row: resolving must stay read-only.
func (c *ServerContext) ResolveResourceID() string {
	if c.Model == nil || !c.Model.Config.Singleton || c.ResourceID != SingletonID {
		return c.ResourceID
	}
	var forced []*FilterExpr
	if c.Query != nil {
		forced = forcedFilters(c.Query.Filters)
	}
	if len(forced) == 0 {
		return c.ResourceID // unscoped: the placeholder *is* the row's id
	}
	exec := dbExec{adapter: c.Model.ResolveAdapter(c.adapter), tx: c.Tx}
	id, err := findScopedSingletonID(c.Ctx, exec, c.Model, forced)
	if err != nil {
		return ""
	}
	return id
}

// singletonRecordID reads a record's primary key, whether the adapter handed back
// a typed *T or a map.
func singletonRecordID(model *ModelMeta, rec any) string {
	m := recordToMap(model, rec)
	if m == nil {
		return ""
	}
	id, _ := m["id"].(string)
	return id
}

// resolveContentType decides an uploaded part's media type and rewinds the
// reader so the body can still be stored in full.
//
// The type the client declared wins. Sniffing (http.DetectContentType) is only a
// fallback for a part that declares nothing, or the generic
// application/octet-stream that clients send when they don't know better — it
// recognises a short allowlist of magic numbers and answers text/plain or
// application/octet-stream for everything else, so letting it override the
// declared type made mfx:"accept" unsatisfiable for JSON, CSV, and every office
// format: the client sent the right header and still got a 415 (BUG-6).
func resolveContentType(declared string, r io.ReadSeeker) (string, error) {
	if ct := strings.TrimSpace(declared); ct != "" &&
		!strings.EqualFold(mediaType(ct), "application/octet-stream") {
		return ct, nil
	}

	head := make([]byte, 512)
	n, err := r.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if n == 0 {
		return declared, nil // empty part — nothing to sniff
	}
	return http.DetectContentType(head[:n]), nil
}

// mediaType strips any MIME parameters ("text/html; charset=utf-8" → "text/html").
func mediaType(contentType string) string {
	return strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
}

// matchesAccept checks if contentType matches any of the accept patterns.
// Supports exact match ("image/png"), wildcard subtype ("image/*"), and
// ignores MIME parameters ("text/html; charset=utf-8" matches "text/html").
// Returns true when accept is empty (no restriction).
func matchesAccept(contentType string, accept []string) bool {
	if len(accept) == 0 {
		return true
	}
	mimeType := mediaType(contentType)
	for _, pattern := range accept {
		if pattern == "*/*" || pattern == mimeType {
			return true
		}
		if strings.HasSuffix(pattern, "/*") {
			prefix := strings.TrimSuffix(pattern, "/*")
			if strings.HasPrefix(mimeType, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// ── DB ────────────────────────────────────────────────────────────────────────
// Dispatches to the configured DBAdapter, or through ctx.Tx when a transaction
// is active.

func (s *defaultSteps) db(ctx *ServerContext, next func() error) error {
	// A query-cache middleware (db.CacheQuery) may have pre-populated DBResult
	// from a Before-DB cache hit. Honour it and skip the adapter read entirely;
	// the Response step builds the envelope from the cached result exactly as it
	// would for a fresh read. Nothing in the default pipeline sets DBResult
	// before this step, so this only fires for such opt-in middleware.
	if ctx.DBResult != nil {
		return next()
	}

	adapter := ctx.Model.ResolveAdapter(s.adapter)
	if adapter == nil {
		ctx.Abort(http.StatusInternalServerError, "NO_ADAPTER", "no database adapter configured")
		return nil
	}

	// Aggregate endpoint (4.7): run the parsed AggregateQuery instead of a list
	// read. ctx.Aggregate resolves its own adapter/transaction, so the standard
	// dispatch below is skipped entirely for this request.
	if ctx.aggregate {
		return s.aggregateDB(ctx, next)
	}

	// Transaction boundary trace: note when the DB step runs inside a tx.
	if ctx.trace != nil && ctx.trace.Steps && ctx.Tx != nil {
		ctx.Logger().Debug("DB step using active transaction")
	}

	// Every filter the request carries is now final: the URL's, which were parsed
	// and so have a known operator, and every one a middleware appended, which was
	// built in Go and has whatever operator was typed. An operator no adapter
	// implements used to be dropped from the WHERE clause, which for a forced
	// filter meant the scope silently did not exist — see validateFilterOperators.
	if ctx.Query != nil {
		if err := validateFilterOperators(ctx.Query.Filters); err != nil {
			ctx.Abort(http.StatusInternalServerError, "INVALID_FILTER", err.Error())
			return nil
		}
	}

	// Route through the active transaction when one is set, so all DB
	// operations within this request use the same connection and isolation.
	exec := dbExec{adapter: adapter, tx: ctx.Tx}

	model := ctx.Model

	// Singleton models: the handler pinned ResourceID to SingletonID but the row
	// may not exist yet (no POST endpoint). Provision it from column defaults so
	// the read/update has a row to act on — this is what makes GET /config return
	// defaults on first call and PATCH /config behave like an upsert.
	if model.Config.Singleton && (ctx.Operation == OpRead || ctx.Operation == OpUpdate) {
		if err := s.ensureSingletonRow(ctx, exec, model); err != nil {
			ctx.Abort(http.StatusInternalServerError, "DB_ERROR", err.Error())
			return nil
		}
	}

	// lock_when: a delete on a record matching any lock condition is rejected
	// just like an update. The Validate step covers updates but does not run
	// for OpDelete, so we mirror the check here just before the adapter call.
	if ctx.Operation == OpDelete && len(model.LockWhen) > 0 {
		if resp := s.checkRecordLocked(ctx); resp != nil {
			ctx.Response = resp
			return nil
		}
	}

	// Optimistic concurrency: when the model opts in and the client supplies an
	// If-Match header, re-read the current record under a row lock, compute its
	// ETag (MD5 of the JSON-mapped body — identical to what response.Cache sets),
	// and reject mismatches with 412. The lock and the write below share one
	// transaction, so a concurrent writer holding the same ETag cannot pass its
	// own check until this one commits — at which point it sees the new ETag and
	// gets its 412 instead of silently overwriting us.
	//
	// ownTx is the transaction this step opened for that guard (nil when the
	// request already had one from WithTransaction, which we join instead).
	var ownTx Tx
	defer func() {
		if ownTx != nil {
			_ = ownTx.Rollback() // no-op once committed
			ctx.Tx = nil
		}
	}()
	var lockErr error
	exec, ownTx, lockErr = s.enforceOptimisticLock(ctx, exec, model)
	if lockErr != nil {
		return lockErr
	}
	if ctx.Response != nil {
		return nil // 412 or 404 was set
	}

	// Row-level scoping: a forced filter (db.Tenancy, db.ForceFilter) constrains
	// reads through ctx.Query, but the adapter's Update/Delete take no filter, so
	// without this the write reaches a row the same filter hides.
	//
	// Runs after the If-Match guard, and at most one of the two opens a
	// transaction — whichever gets there first leaves ctx.Tx set, and the other
	// joins it. Its tx folds into ownTx so there stays exactly one owner, one
	// commit below, and one rollback in the defer above: a transaction opened
	// here but committed nowhere would roll back at return and silently discard
	// the very write it just authorised.
	var scopeTx Tx
	var scopeErr error
	exec, scopeTx, scopeErr = s.enforceWriteScope(ctx, exec, model)
	if scopeTx != nil {
		ownTx = scopeTx
	}
	if scopeErr != nil {
		return scopeErr
	}
	if ctx.Response != nil {
		return nil // 404 was set — the record is out of scope
	}

	// A scope that runs through a parent (db.ForceFilterVia) hangs off a foreign
	// key the client supplies, and neither the check above nor the read path looks
	// at it: a create names a parent nothing has read, and an update can rewrite
	// the key after the row was checked where it used to be. Same transaction, same
	// fold — see enforceParentScope.
	var parentTx Tx
	var parentErr error
	exec, parentTx, parentErr = s.enforceParentScope(ctx, exec, model)
	if parentTx != nil {
		ownTx = parentTx
	}
	if parentErr != nil {
		return parentErr
	}
	if ctx.Response != nil {
		return nil // 404/422 was set — the parent is out of scope, or absent
	}

	// Cascading deletes: on OpDelete, apply each onDelete action to this row's
	// children (cascade/setNull/restrict) before the row itself is deleted, in the
	// same transaction so the whole deletion is atomic (5.16). Same tx fold as
	// above — exactly one of these guards opens ownTx and the rest join it.
	var cascadeTx Tx
	var cascadeErr error
	exec, cascadeTx, cascadeErr = s.enforceCascadeDelete(ctx, exec, model)
	if cascadeTx != nil {
		ownTx = cascadeTx
	}
	if cascadeErr != nil {
		return cascadeErr
	}
	if ctx.Response != nil {
		return nil // 409 was set — a restrict edge refused the delete
	}

	// Build the DB-column-keyed write map. With the body-mutating middleware
	// migrated to SetField/DeleteField (W1/W2), the bound typed record carries
	// the authoritative write set in the common case, so we source columns from
	// it. ParsedBody remains the fallback for writes the record can't faithfully
	// represent — raw ParsedBody mutation that bypassed SetField, loose-typed or
	// multipart values that never bound to the struct (see recordSourcedWrite).
	var dbData map[string]any
	if recordSourcedWrite(ctx, model) {
		dbData = recordToMap(model, ctx.Record)
	} else {
		dbData = toDBMap(ctx, ctx.ParsedBody, model)
	}

	// Encryption: guard writes and encrypt field values before they reach the adapter.
	if ctx.Operation == OpCreate || ctx.Operation == OpUpdate {
		if model.HasEncryptedFields() {
			if s.keyProvider == nil {
				for _, f := range model.EncryptedFields() {
					if _, ok := dbData[f.Tags.DBName]; ok {
						abortEncryptionNotConfigured(ctx, f.Tags.DBName)
						return nil
					}
				}
			} else {
				if err := encryptFields(ctx.Ctx, s.keyProvider, model, dbData); err != nil {
					ctx.Abort(http.StatusInternalServerError, "ENCRYPTION_ERROR", err.Error())
					return nil
				}
			}
		}
	}

	var dbErr error

	switch ctx.Operation {
	case OpList, OpExport:
		// Reuse the list query path; the only difference for OpExport is that
		// pagination is disabled (Limit is overridden to MaxExportRows). The
		// response step branches on Operation to choose the wire format.
		q := ctx.Query
		if ctx.Operation == OpExport {
			cap := model.Config.MaxExportRows
			if cap <= 0 {
				cap = DefaultMaxExportRows
			}
			q = &QueryParams{
				Page:     1,
				Limit:    cap + 1, // +1 sentinel so we can detect overrun
				Filters:  ctx.Query.Filters,
				Sorts:    ctx.Query.Sorts,
				Includes: ctx.Query.Includes,
				Fields:   ctx.Query.Fields,
				Search:   ctx.Query.Search, // honour ?q= full-text search on exports too
			}
		}
		var items []any
		var total int64
		items, total, dbErr = exec.findManyTyped(ctx.Ctx, model, q)
		if dbErr == nil {
			// Encrypted models decrypt on a map view; the common path keeps the
			// typed *T records for marshalRecord.
			if model.HasEncryptedFields() {
				for i, rec := range items {
					row := recordToMap(model, rec)
					if s.keyProvider != nil {
						if err := decryptFields(ctx.Ctx, s.keyProvider, model, row); err != nil {
							ctx.Abort(http.StatusInternalServerError, "DECRYPTION_ERROR", err.Error())
							return nil
						}
					} else {
						stripHMACColumns(model, row)
					}
					items[i] = row
				}
			}
			if ctx.Operation == OpExport {
				cap := model.Config.MaxExportRows
				if cap <= 0 {
					cap = DefaultMaxExportRows
				}
				if len(items) > cap {
					ctx.Abort(http.StatusRequestEntityTooLarge, "EXPORT_TOO_LARGE",
						fmt.Sprintf("export exceeds %d rows; tighten filters", cap))
					return nil
				}
			}
			ctx.DBResult = &ListResult{Items: items, Total: total, Query: q}
		}

	case OpReadHistory:
		dbErr = s.readHistory(ctx, exec, model)

	case OpRead, OpReadAttachment:
		var rec any
		rec, dbErr = exec.findByIDTyped(ctx.Ctx, model, ctx.ResourceID, ctx.Query)
		if dbErr == nil {
			// Encryption and attachment streaming consume a map; the common JSON
			// read path keeps the typed *T so marshalRecord reads struct fields.
			if model.HasEncryptedFields() || ctx.Operation == OpReadAttachment {
				result := recordToMap(model, rec)
				if model.HasEncryptedFields() {
					if s.keyProvider != nil {
						if err := decryptFields(ctx.Ctx, s.keyProvider, model, result); err != nil {
							ctx.Abort(http.StatusInternalServerError, "DECRYPTION_ERROR", err.Error())
							return nil
						}
					} else {
						stripHMACColumns(model, result)
					}
				}
				ctx.DBResult = result
			} else {
				ctx.DBResult = rec
			}
		}

	case OpCreate:
		// lock_scope: acquire FOR UPDATE on referenced rows before insert.
		// Requires an active transaction so the lock is held until commit.
		if len(model.LockScopes) > 0 {
			if ctx.Tx == nil {
				ctx.Abort(http.StatusInternalServerError, "LOCK_SCOPE_NO_TX",
					"models with lock_scope fields require an active transaction; register maniflex.WithTransaction(nil) on the Service step")
				return nil
			}
			for _, ls := range model.LockScopes {
				refIDVal := dbData[ls.DBName]
				if refIDVal == nil {
					continue
				}
				refID := fmt.Sprint(refIDVal)
				if refID == "" {
					continue
				}
				refMeta, ok := s.reg.Get(ls.Model)
				if !ok {
					ctx.Abort(http.StatusInternalServerError, "LOCK_SCOPE_ERROR",
						fmt.Sprintf("lock_scope model %q not registered", ls.Model))
					return nil
				}
				if _, err := ctx.Tx.FindByIDForUpdate(ctx.Ctx, refMeta, refID); err != nil {
					if errors.Is(err, ErrNotFound) {
						ctx.Abort(http.StatusNotFound, "NOT_FOUND",
							fmt.Sprintf("%s with id %q not found", ls.Model, refID))
						return nil
					}
					ctx.Abort(http.StatusInternalServerError, "DB_ERROR", err.Error())
					return nil
				}
			}
		}
		var result map[string]any
		result, dbErr = exec.Create(ctx.Ctx, model, dbData)
		if dbErr == nil {
			if model.HasEncryptedFields() {
				if s.keyProvider != nil {
					if err := decryptFields(ctx.Ctx, s.keyProvider, model, result); err != nil {
						ctx.Abort(http.StatusInternalServerError, "DECRYPTION_ERROR", err.Error())
						return nil
					}
				} else {
					stripHMACColumns(model, result)
				}
			}
			ctx.DBResult = result
		}

	case OpUpdate:
		var result map[string]any
		if ctx.restore {
			// A restore (5.19) rides OpUpdate for its middleware, but writes
			// through its own statement: the row is soft-deleted, so Update
			// cannot see it.
			result, dbErr = s.restoreRow(ctx, exec, model)
			if dbErr == nil && ctx.Response != nil {
				return nil // 501: the adapter cannot restore
			}
		} else {
			result, dbErr = exec.Update(ctx.Ctx, model, ctx.ResourceID, dbData)
		}
		if dbErr == nil {
			if model.HasEncryptedFields() {
				if s.keyProvider != nil {
					if err := decryptFields(ctx.Ctx, s.keyProvider, model, result); err != nil {
						ctx.Abort(http.StatusInternalServerError, "DECRYPTION_ERROR", err.Error())
						return nil
					}
				} else {
					stripHMACColumns(model, result)
				}
			}
			ctx.DBResult = result
		}

	case OpDelete:
		dbErr = exec.Delete(ctx.Ctx, model, ctx.ResourceID)
	}

	if dbErr != nil {
		if errors.Is(dbErr, ErrNotFound) {
			ctx.Abort(http.StatusNotFound, "NOT_FOUND",
				fmt.Sprintf("%s with id %q not found", model.Name, ctx.ResourceID))
			return nil
		}
		// A deadline or cancellation from ctx.Ctx means the per-request
		// QueryTimeout fired: return 504 so the caller knows the query was cut
		// short rather than seeing a generic 500 with an opaque driver error
		// message. If instead the caller hung up, the query was cut short by
		// them — that is a 499, not a timeout we are answerable for.
		if errors.Is(dbErr, context.DeadlineExceeded) || errors.Is(dbErr, context.Canceled) {
			if clientGone(ctx) {
				ctx.Response = clientGoneResponse()
				return nil
			}
			ctx.Abort(http.StatusGatewayTimeout, "TIMEOUT",
				"request exceeded the configured query timeout")
			return nil
		}
		// Unwrap wrapped constraint errors (e.g. fmt.Errorf("create: %w", *ErrConstraint))
		var constraintErr *ErrConstraint
		if errors.As(dbErr, &constraintErr) {
			// A foreign-key violation from a database-enforced onDelete constraint
			// (5.16). On a delete it is a restrict edge refusing the parent — the
			// same 409 the maniflex-layer restrict returns, so the two enforcement
			// paths answer alike. On a create/update it is a child naming a parent
			// that does not exist.
			if constraintErr.Kind == ConstraintForeignKey {
				if ctx.Operation == OpDelete {
					ctx.Response = &APIResponse{
						StatusCode: http.StatusConflict,
						Error: &APIError{
							Code:    "DELETE_RESTRICTED",
							Message: "cannot delete: the record is still referenced by related records",
						},
					}
				} else {
					ctx.Response = &APIResponse{
						StatusCode: http.StatusConflict,
						Error: &APIError{
							Code:    "FOREIGN_KEY_VIOLATION",
							Message: "references a related record that does not exist",
						},
					}
				}
				return nil
			}
			// A NOT NULL violation is a missing-required-value problem, not a
			// conflict — surface it as a 422 validation error (matching the
			// validate middleware) instead of an opaque 500 or a misleading 409.
			if constraintErr.Kind == ConstraintNotNull {
				details := map[string]string{"message": "value is required"}
				if constraintErr.Column != "" {
					details["field"] = constraintErr.Column
					details["message"] = constraintErr.Column + " is required"
				}
				ctx.Response = &APIResponse{
					StatusCode: http.StatusUnprocessableEntity,
					Error: &APIError{
						Code:    "VALIDATION_ERROR",
						Message: "missing required field",
						Details: details,
					},
				}
				return nil
			}
			details := map[string]string{
				"message": "value already exists",
			}
			if constraintErr.Column != "" {
				details["field"] = strings.Split(constraintErr.Column, " ")[0]
				details["message"] = constraintErr.Column + " already taken"
			}
			ctx.Response = &APIResponse{
				StatusCode: http.StatusConflict,
				Error: &APIError{
					Code:    "CONFLICT",
					Message: "unique constraint violation",
					Details: details,
				},
			}
			return nil
		}
		ctx.Abort(http.StatusInternalServerError, "DB_ERROR", dbErr.Error())
		return nil
	}

	// The optimistic-lock guard held the row from the ETag check through the
	// write; commit so both land atomically and the lock is released before the
	// After-DB middleware run (they see a committed row and a nil ctx.Tx, exactly
	// as they did when the guard used no transaction at all).
	if ownTx != nil {
		if err := ownTx.Commit(); err != nil {
			ctx.Abort(http.StatusInternalServerError, "TX_COMMIT_ERROR",
				fmt.Sprintf("failed to commit transaction: %v", err))
			return nil
		}
		ownTx = nil
		ctx.Tx = nil
	}

	return next()
}

// dbExec selects between the transaction and the bare adapter.
// When tx is non-nil all calls are routed through it; otherwise the adapter
// is used directly. This avoids a long if/else chain in every operation.
type dbExec struct {
	adapter DBAdapter
	tx      Tx
}

// restoreRow clears a soft-delete marker for the restore route (5.19).
//
// The scope the request carries is pushed into the statement rather than
// checked with a read first, because the target is soft-deleted and therefore
// invisible to every read path — see the note in enforceWriteScope. Only forced
// filters travel, matching how writes are scoped elsewhere: a client's own
// ?filter= has never constrained a write.
//
// It aborts with 501 when the adapter cannot restore, and returns ErrNotFound
// (which the caller renders as 404) when no row matches — including a row that
// exists but is not deleted.
func (s *defaultSteps) restoreRow(ctx *ServerContext, exec dbExec, model *ModelMeta) (map[string]any, error) {
	restorer, ok := exec.restorer()
	if !ok {
		ctx.Abort(http.StatusNotImplemented, "RESTORE_UNSUPPORTED",
			"the configured database adapter does not support restoring soft-deleted records")
		return nil, nil
	}

	var scoped *QueryParams
	if ctx.Query != nil {
		if forced := forcedFilters(ctx.Query.Filters); len(forced) > 0 {
			scoped = &QueryParams{Filters: forced}
		}
	}
	v, err := restorer.Restore(ctx.Ctx, model, ctx.ResourceID, scoped)
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(model, v), nil
}

// restorer reports the Restorer behind this exec — the transaction when the
// request runs in one, else the adapter — and whether it supports restoring at
// all.
// scopeChecker reports the ScopeChecker behind this exec — the transaction when
// the request runs in one, else the adapter — and whether it supports the
// soft-delete-inclusive scope probe at all.
func (e dbExec) scopeChecker() (ScopeChecker, bool) {
	if e.tx != nil {
		s, ok := e.tx.(ScopeChecker)
		return s, ok
	}
	s, ok := e.adapter.(ScopeChecker)
	return s, ok
}

func (e dbExec) restorer() (Restorer, bool) {
	if e.tx != nil {
		r, ok := e.tx.(Restorer)
		return r, ok
	}
	r, ok := e.adapter.(Restorer)
	return r, ok
}

// dbExec presents a map-based facade over the now-typed DBAdapter/Tx interface
// (Phase 3 bridge). Each method calls the typed method and bridges *T↔map so the
// still-map pipeline is unchanged. Removed in Phase 7 when the pipeline is typed.
func (e dbExec) FindByID(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (map[string]any, error) {
	var v any
	var err error
	if e.tx != nil {
		v, err = e.tx.FindByID(ctx, model, id, q)
	} else {
		v, err = e.adapter.FindByID(ctx, model, id, q)
	}
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(model, v), nil
}

func (e dbExec) FindMany(ctx context.Context, model *ModelMeta, q *QueryParams) ([]map[string]any, int64, error) {
	var items []any
	var total int64
	var err error
	if e.tx != nil {
		items, total, err = e.tx.FindMany(ctx, model, q)
	} else {
		items, total, err = e.adapter.FindMany(ctx, model, q)
	}
	if err != nil {
		return nil, 0, err
	}
	maps := make([]map[string]any, len(items))
	for i, it := range items {
		maps[i] = recordToMap(model, it)
	}
	return maps, total, nil
}

func (e dbExec) Create(ctx context.Context, model *ModelMeta, data map[string]any) (map[string]any, error) {
	rec, err := mapToRecord(model, data)
	if err != nil {
		return nil, err
	}
	var v any
	if e.tx != nil {
		v, err = e.tx.Create(ctx, model, rec)
	} else {
		v, err = e.adapter.Create(ctx, model, rec)
	}
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(model, v), nil
}

func (e dbExec) Update(ctx context.Context, model *ModelMeta, id string, data map[string]any) (map[string]any, error) {
	rec, err := mapToRecord(model, data)
	if err != nil {
		return nil, err
	}
	present := presentDBKeys(data)
	var v any
	if e.tx != nil {
		v, err = e.tx.Update(ctx, model, id, rec, present)
	} else {
		v, err = e.adapter.Update(ctx, model, id, rec, present)
	}
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(model, v), nil
}

// findByIDForUpdate reads a single record and acquires a pessimistic row lock on
// it (Postgres FOR UPDATE; on SQLite the write connection is already serialized).
// The lock only outlives the call when e.tx is set, so callers that depend on it
// must run inside a transaction.
func (e dbExec) findByIDForUpdate(ctx context.Context, model *ModelMeta, id string) (map[string]any, error) {
	var v any
	var err error
	if e.tx != nil {
		v, err = e.tx.FindByIDForUpdate(ctx, model, id)
	} else {
		v, err = e.adapter.FindByIDForUpdate(ctx, model, id)
	}
	if err != nil || v == nil {
		return nil, err
	}
	return recordToMap(model, v), nil
}

func (e dbExec) Delete(ctx context.Context, model *ModelMeta, id string) error {
	if e.tx != nil {
		return e.tx.Delete(ctx, model, id)
	}
	return e.adapter.Delete(ctx, model, id)
}

// findByIDTyped / findManyTyped return the adapter's record carriers (*T)
// without the map bridge, so the read path can hand a field-populated record
// straight to marshalRecord (the typed-models fast path). Writes still use the
// map-bridged methods above during the transition.
func (e dbExec) findByIDTyped(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (any, error) {
	if e.tx != nil {
		return e.tx.FindByID(ctx, model, id, q)
	}
	return e.adapter.FindByID(ctx, model, id, q)
}

func (e dbExec) findManyTyped(ctx context.Context, model *ModelMeta, q *QueryParams) ([]any, int64, error) {
	if e.tx != nil {
		return e.tx.FindMany(ctx, model, q)
	}
	return e.adapter.FindMany(ctx, model, q)
}

// ── Response ──────────────────────────────────────────────────────────────────
// Builds the JSON response envelope from ctx.DBResult.

func (s *defaultSteps) response(ctx *ServerContext, next func() error) error {
	// A previous step may have already set ctx.Response (e.g. an error abort).
	if ctx.Response != nil {
		return next()
	}

	// Action handlers set ctx.Response directly. If they didn't, default to 200 OK with no body.
	if ctx.Operation == OpAction {
		ctx.Response = &APIResponse{StatusCode: http.StatusOK}
		return next()
	}

	// Aggregate endpoint (4.7): the DB step put the group rows ([]Row) in
	// ctx.DBResult. Emit them as a plain JSON array under the standard
	// {"data": ...} envelope — no pagination meta or per-record marshalling.
	if ctx.aggregate {
		rows := ctx.DBResult
		if rows == nil {
			rows = []Row{}
		}
		ctx.Response = &APIResponse{StatusCode: http.StatusOK, Data: rows}
		return next()
	}

	// Per-model attachment route (3B.3a): dereference the storage key from
	// ctx.DBResult and stream the referenced file directly to the response.
	// ctx.Response stays nil so dispatch() does not write a JSON envelope on
	// top of the streamed body.
	if ctx.Operation == OpReadAttachment {
		s.streamAttachment(ctx)
		return next()
	}

	// CSV/XLSX export (8.3): stream the bytes directly. Skip the JSON envelope
	// for the same reason as attachments — ctx.Response stays nil.
	if ctx.Operation == OpExport {
		s.streamExportRows(ctx)
		return next()
	}

	// OPTIONS never reads anything: answer 204 with the Allow header the handler
	// already set. Returning here — before the DBResult check below — is also what
	// keeps it out of the "reached with nil DBResult" warning, which used to fire
	// on every OPTIONS request and filled the logs at probe frequency (BUG-9).
	if ctx.Operation == OpOptions {
		ctx.Response = &APIResponse{StatusCode: http.StatusNoContent}
		return next()
	}

	// Needed in case of `maniflex.Pipeline.DB.Register(..., maniflex.AtPosition(maniflex.Replace))`
	// and ctx.DBResult wasn't manually set
	if ctx.DBResult == nil {
		ctx.Logger().Warn("Response step reached with ctx.DBResult == nil")
		ctx.DBResult = &ListResult{Items: []any{}, Total: 0, Query: ctx.Query}
	}
	model := ctx.Model

	switch ctx.Operation {
	case OpDelete:
		ctx.Response = &APIResponse{StatusCode: http.StatusNoContent}
	case OpCreate:
		s.recordResponse(ctx, model, http.StatusCreated)
	case OpList:
		s.listResponse(ctx, model)
	case OpReadHistory:
		// The rows are history rows, so they must be projected through the
		// history model's fields — marshalling them against the parent would
		// match almost nothing and emit empty objects. ctx.Model stays the
		// parent throughout, because that is what every middleware is scoped to.
		if hist, ok := s.reg.Get(model.Name + "History"); ok {
			s.listResponse(ctx, hist)
		} else {
			s.listResponse(ctx, model)
		}
	default: // OpRead, OpUpdate
		s.recordResponse(ctx, model, http.StatusOK)
	}

	return next()
}

// recordResponse builds the single-record envelope for create, read, and update.
func (s *defaultSteps) recordResponse(ctx *ServerContext, model *ModelMeta, status int) {
	if !marshalableRecord(ctx.DBResult) {
		abortInvalidDBResult(ctx, "a record — map[string]any or *T", ctx.DBResult)
		return
	}
	row := s.marshalRecord(model, ctx.DBResult, ctx)
	s.rewriteFileACL(ctx.Ctx, model, row)
	applyComputed(ctx, model, ctx.DBResult, row)
	resp := &APIResponse{StatusCode: status, Data: row}
	if v, ok := ctx.Get("_rtl"); ok && v == true {
		resp.Dir = "rtl"
	}
	ctx.Response = resp
}

// listResponse builds the list envelope and its pagination meta.
func (s *defaultSteps) listResponse(ctx *ServerContext, model *ModelMeta) {
	lr, ok := ctx.DBResult.(*ListResult)
	if !ok {
		abortInvalidDBResult(ctx, "*maniflex.ListResult", ctx.DBResult)
		return
	}
	// A hand-built ListResult from a Replace middleware typically carries only
	// Items and Total. Fill in the pagination the meta needs rather than
	// nil-dereferencing an absent Query or dividing by a zero Limit (BUG-13).
	q := lr.normalizeQuery()

	items := make([]any, len(lr.Items))
	for i, row := range lr.Items {
		m := s.marshalRecord(model, row, ctx)
		s.rewriteFileACL(ctx.Ctx, model, m)
		items[i] = m
	}
	applyComputedList(ctx, model, items, lr.Items)

	var meta *ResponseMeta
	if c := q.Cursor; c != nil {
		// Keyset mode: no total/page/pages — report the next-page token and
		// whether more rows follow instead.
		meta = &ResponseMeta{
			Cursor:     true,
			Limit:      q.Limit,
			NextCursor: c.NextCursor,
			HasMore:    c.HasMore,
		}
	} else {
		pages := lr.Total / int64(q.Limit)
		if lr.Total%int64(q.Limit) != 0 {
			pages++
		}
		meta = &ResponseMeta{
			Total: lr.Total,
			Page:  q.Page,
			Limit: q.Limit,
			Pages: pages,
		}
	}
	if v, ok := ctx.Get("_rtl"); ok && v == true {
		meta.Dir = "rtl"
	}
	ctx.Response = &APIResponse{
		StatusCode: http.StatusOK,
		Data:       items,
		Meta:       meta,
	}
}

// marshalableRecord reports whether v is something marshalRecord can render: a
// map[string]any, or a non-nil pointer to a record struct. Anything else — a
// value struct, an int, a nil pointer — used to reach reflect.Value.Elem and
// panic the Response step, which a Replace middleware could trigger from Go code
// that looks perfectly reasonable (BUG-13).
func marshalableRecord(v any) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(map[string]any); ok {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && !rv.IsNil()
}

// abortInvalidDBResult reports a ctx.DBResult the Response step cannot render.
// It names the type it got, because the only way to land here is a middleware
// that replaced a step and set the wrong shape — so say what was expected.
func abortInvalidDBResult(ctx *ServerContext, want string, got any) {
	ctx.Abort(http.StatusInternalServerError, "INVALID_DB_RESULT",
		fmt.Sprintf("%s response expects %s in ctx.DBResult, got %T",
			ctx.Operation, want, got))
}

// streamExportRows serves the auto-generated CSV/XLSX export route. The DB
// step has already populated ctx.DBResult with a *ListResult, capped at the
// model's MaxExportRows. We parse ?format, set the Content-Disposition
// header, and write the body directly to ctx.Writer. ctx.Response stays nil
// so dispatch() does not double-write.
func (s *defaultSteps) streamExportRows(ctx *ServerContext) {
	format, err := parseExportFormat(ctx.Request.URL.Query().Get("format"))
	if err != nil {
		ctx.Abort(http.StatusBadRequest, "INVALID_FORMAT", err.Error())
		return
	}
	lr, ok := ctx.DBResult.(*ListResult)
	if !ok {
		ctx.Abort(http.StatusInternalServerError, "EXPORT_NO_RESULT",
			"DB step did not produce a ListResult")
		return
	}

	model := ctx.Model
	fields := exportColumns(model, ctx)

	if err := streamExport(ctx.Writer, model.Name, format, fields, s.exportRowSeq(ctx, model, lr)); err != nil {
		ctx.Logger().Warn("export write failed", "error", err.Error())
	}
}

// exportRowSeq yields each export row, serialising a record only as the writer
// asks for it. Building the whole []map[string]any up front held a second copy
// of the entire result set — on top of the typed records — for the length of the
// write, though each row is used once and then never again. A row's map is now
// garbage as soon as it has been written.
//
// A model with computed fields resolves them a chunk at a time instead: a batch
// resolver then costs one call per computedExportChunk records rather than one
// per record, and only a chunk's maps are live at once. A model with no computed
// fields never buffers a chunk.
func (s *defaultSteps) exportRowSeq(ctx *ServerContext, model *ModelMeta, lr *ListResult) func(func(map[string]any) bool) {
	if hasComputedFields(model) {
		return s.exportRowSeqComputed(ctx, model, lr)
	}
	return func(yield func(map[string]any) bool) {
		for _, rec := range lr.Items {
			if !yield(s.exportRow(ctx, model, rec)) {
				return
			}
		}
	}
}

// exportRowSeqComputed is exportRowSeq for a model carrying computed fields:
// same laziness, but resolved a chunk at a time.
func (s *defaultSteps) exportRowSeqComputed(ctx *ServerContext, model *ModelMeta, lr *ListResult) func(func(map[string]any) bool) {
	return func(yield func(map[string]any) bool) {
		for start := 0; start < len(lr.Items); start += computedExportChunk {
			recs := lr.Items[start:min(start+computedExportChunk, len(lr.Items))]
			maps := make([]map[string]any, len(recs))
			for i, rec := range recs {
				maps[i] = s.exportRow(ctx, model, rec)
			}
			applyComputedRows(ctx, model, maps, recs)
			for _, m := range maps {
				if !yield(m) {
					return
				}
			}
		}
	}
}

// exportRow serialises one record to its export row, computed fields aside.
func (s *defaultSteps) exportRow(ctx *ServerContext, model *ModelMeta, rec any) map[string]any {
	m := s.marshalRecord(model, rec, ctx)
	s.rewriteFileACL(ctx.Ctx, model, m)
	return m
}

// streamAttachment serves the per-model attachment route. It dereferences the
// file key from ctx.DBResult, fetches the file from storage, and streams it
// directly to ctx.Writer. On any error it calls ctx.Abort, which the response
// step picks up on the next pass — we leave ctx.Response nil on success so
// dispatch() does not double-write.
func (s *defaultSteps) streamAttachment(ctx *ServerContext) {
	if ctx.AttachmentField == nil {
		ctx.Abort(http.StatusInternalServerError, "ATTACHMENT_FIELD_MISSING",
			"attachment field not set on context")
		return
	}
	if s.storage == nil {
		ctx.Abort(http.StatusNotImplemented, "NO_STORAGE", "file storage not configured")
		return
	}
	record, ok := ctx.DBResult.(map[string]any)
	if !ok {
		ctx.Abort(http.StatusInternalServerError, "ATTACHMENT_NO_RECORD",
			"DB step did not produce a record")
		return
	}
	key, _ := record[ctx.AttachmentField.Tags.DBName].(string)
	if key == "" {
		ctx.Abort(http.StatusNotFound, "FILE_NOT_SET",
			fmt.Sprintf("field %q has no file", ctx.AttachmentField.Tags.JSONName))
		return
	}

	if ferr := serveStoredFile(ctx.Ctx, ctx.Writer, ctx.Request, s.storage, key); ferr != nil {
		ctx.Abort(ferr.Status, ferr.Code, ferr.Message)
		return
	}
	// ctx.Response intentionally left nil — the body is already on the wire.
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// recordSourcedWrite reports whether the bound typed record (ctx.Record)
// faithfully covers the request body, so the DB write can source column values
// from the struct instead of from ParsedBody. It requires a typed carrier whose
// present-set exactly matches the DB-column keys ParsedBody would produce. Any
// divergence — a raw ParsedBody mutation that bypassed SetField, or loose-typed
// / multipart values that never bound to the struct — fails the check and the
// caller falls back to toDBMap(ParsedBody). This keeps write-column parity with
// the map path exactly while moving the common case onto the record.
func recordSourcedWrite(ctx *ServerContext, model *ModelMeta) bool {
	if ctx.Record == nil || model.GoType == nil {
		return false
	}
	present := PresentColumns(ctx.Record)
	if present == nil {
		return false
	}
	return bodyColumnsMatch(ctx.ParsedBody, model, present)
}

// bodyColumnsMatch reports whether the body names exactly the DB columns in
// present. It walks the body's columns rather than materialising them: this decides
// which source a write is read from on every create and update, and it only ever
// needed the key set — building the whole map meant converting every value and
// JSON-marshalling every locale field just to throw the result away, and on the
// path where the answer is "no" the caller then built the very same map again
// (PERF-4). The fallback map is now built once, by the caller that uses it.
func bodyColumnsMatch(b *RequestBody, model *ModelMeta, present map[string]struct{}) bool {
	n := 0
	for _, f := range model.Fields {
		if _, ok := b.Get(f.Tags.JSONName); !ok {
			continue
		}
		if _, inPresent := present[f.Tags.DBName]; !inPresent {
			return false
		}
		n++
	}
	// Every body column is present; equal counts then mean the sets match exactly.
	return n == len(present)
}

// toDBMap converts a JSON-keyed body map to a DB-column-keyed map.
// Only fields present in the model are included. LocaleString fields are
// marshalled to a JSON string so database/sql can store them as TEXT/JSONB.
func toDBMap(ctx *ServerContext, b *RequestBody, model *ModelMeta) map[string]any {
	raw := b.raw()
	if raw == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(raw))
	for i := range model.Fields {
		f := &model.Fields[i]
		if f.Tags.Locale {
			if v, ok := localeWriteValue(ctx, f, model, b); ok {
				if encoded, err := json.Marshal(v); err == nil {
					out[f.Tags.DBName] = string(encoded)
				}
			}
			continue
		}
		if v, ok := b.Get(f.Tags.JSONName); ok {
			out[f.Tags.DBName] = v
		}
	}
	return out
}

// localeWriteValue picks what a locale column should be written from, given a
// request body that may carry either shape split mode emits.
//
// Split mode — the default — answers a read with "name": the resolved string
// for one locale, plus "name_i18n": the full map. Nothing consumed either on
// the way back in: the bare string was marshalled straight into the column as a
// JSON scalar, so the next read could not unmarshal it into map[string]string
// (audit MS-14). That is not a niche path; echoing a response back is what a
// generic edit form does, and one such PATCH left the row unreadable — a 500 on
// the record *and* on the whole collection endpoint, since one bad row fails the
// list scan.
//
// So both shapes are now understood, companion first:
//
//   - "name_i18n" present and parseable → that map wins. It is the complete
//     value, where the bare field is one locale's rendering of it, so an echoed
//     response round-trips without losing the translations it did not show.
//   - "name" holding a bare string → folded into the effective locale's key.
//     This replaces the column rather than merging into the stored map, which is
//     what a map write already does — a locale write has never been a per-key
//     patch, and inventing merge semantics here would make the two shapes behave
//     differently.
//   - anything else → passed through unchanged, as before.
func localeWriteValue(ctx *ServerContext, f *FieldMeta, model *ModelMeta, b *RequestBody) (any, bool) {
	suffix := "_i18n"
	if ctx != nil && ctx.SplitSuffix != "" {
		suffix = ctx.SplitSuffix
	}
	if v, ok := b.Get(f.Tags.JSONName + suffix); ok {
		if m := localeStringToMap(v); m != nil {
			return m, true
		}
	}
	v, ok := b.Get(f.Tags.JSONName)
	if !ok {
		return nil, false
	}
	if s, isString := v.(string); isString {
		// effectiveLocaleChain ends in a hard "en", so there is always a key.
		return map[string]string{effectiveLocaleChain(ctx, f, model)[0]: s}, true
	}
	return v, true
}

// marshalRecord builds the JSON-keyed response map directly from a typed record
// (*T), mirroring toJSONMap exactly but reading each value from the struct field
// (via FieldMeta.Index) or the extra carrier instead of a DB-column map. It is
// byte-equivalent to toJSONMap(recordToMap(record)): the same cast() and locale
// resolution run on the same values, just sourced without the intermediate map.
//
// Value source per column: the extra carrier wins (driver-shaped scalars from a
// bridge record, computed fields, populated includes), otherwise the struct
// field — but only when the column is "present" (the set scanStruct recorded, so
// ?select= projections omit unscanned columns, matching the map path).
//
// A map argument (GoType-less synthetic models, or a record already reduced to a
// map) falls back to toJSONMap.
func (s *defaultSteps) marshalRecord(model *ModelMeta, record any, ctx *ServerContext) map[string]any {
	if record == nil {
		return nil
	}
	if m, ok := record.(map[string]any); ok {
		return s.toJSONMap(m, model, ctx)
	}
	sv := reflect.ValueOf(record).Elem()
	extra := ExtraColumns(record)
	present := PresentColumns(record)

	out := make(map[string]any, len(model.Fields))
	splitSuffix := "_i18n"
	if ctx != nil && ctx.SplitSuffix != "" {
		splitSuffix = ctx.SplitSuffix
	}

	redacted := ctx != nil && len(ctx.redactedFields) > 0
	for i := range model.Fields {
		f := &model.Fields[i]
		if f.Tags.Hidden || f.Tags.WriteOnly {
			continue
		}
		// Declared through ctx.RedactResponseField — a per-request decision the
		// static tags cannot express (audit MS-11).
		if redacted && ctx.IsFieldRedacted(f.Tags.JSONName) {
			continue
		}
		v, ok := recordValue(sv, f, extra, present)
		if !ok {
			continue
		}
		v = derefPtr(v) // pointer scalar fields → their value (the map path is already deref'd)
		if !f.Tags.Locale {
			out[f.Tags.JSONName] = cast(v, f.Type)
			continue
		}

		m, folded := localeValueToMap(v)
		if v != nil && ctx != nil {
			switch {
			case m == nil:
				ctx.Logger().Warn("locale field has unparseable DB value",
					slog.String("model", model.Name),
					slog.String("field", f.Tags.DBName))
			case folded:
				// Readable, but the column is corrupt: it holds a bare scalar,
				// not a locale object. Surfaced rather than silently normalised.
				ctx.Logger().Warn("locale field held a bare scalar, folded to a single locale",
					slog.String("model", model.Name),
					slog.String("field", f.Tags.DBName))
			}
		}
		mode := effectiveLocaleMode(f, model, ctx)
		switch mode {
		case LocaleModeDynamic:
			if ctx != nil && ctx.Locale != "" {
				chain := effectiveLocaleChain(ctx, f, model)
				out[f.Tags.JSONName] = resolveLocaleString(m, chain)
			} else {
				out[f.Tags.JSONName] = localeMapToAny(m)
			}
		case LocaleModeResolve:
			chain := effectiveLocaleChain(ctx, f, model)
			out[f.Tags.JSONName] = resolveLocaleString(m, chain)
		default: // LocaleModeSplit
			chain := effectiveLocaleChain(ctx, f, model)
			out[f.Tags.JSONName] = resolveLocaleString(m, chain)
			out[f.Tags.JSONName+splitSuffix] = localeMapToAny(m)
		}
	}

	// Nested relation objects the adapter populated into the extra carrier.
	for _, rel := range model.Relations {
		if nested, ok := extra[rel.RelationKey]; ok {
			if relModel, ok := s.reg.Get(rel.RelatedModel); ok {
				out[rel.RelationKey] = s.getNestedField(nested, relModel, ctx)
			}
		}
	}
	// Framework-reserved underscore-prefixed keys (e.g. "_through").
	for k, v := range extra {
		if len(k) > 0 && k[0] == '_' {
			out[k] = v
		}
	}
	return out
}

// derefPtr returns the pointed-to value of a pointer (nil for a nil pointer),
// or the value unchanged. The map serialization path receives driver-deref'd
// scalars, so marshalRecord dereferences pointer fields to match.
func derefPtr(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return rv.Elem().Interface()
	}
	return v
}

// recordValue resolves a column's value for marshalRecord: the extra carrier
// takes precedence (it holds whatever the bridge or include population stored),
// otherwise the struct field is read when the column is present. Returns ok=false
// to omit the column entirely (matching toJSONMap's "key absent" behavior).
func recordValue(sv reflect.Value, f *FieldMeta, extra map[string]any, present map[string]struct{}) (any, bool) {
	if v, ok := extra[f.Tags.DBName]; ok {
		return v, true
	}
	if present == nil {
		return sv.FieldByIndex(f.Index).Interface(), true
	}
	if _, ok := present[f.Tags.DBName]; ok {
		return sv.FieldByIndex(f.Index).Interface(), true
	}
	return nil, false
}

// toJSONMap converts a DB-column-keyed result map to a JSON-keyed response map.
// Hidden and write-only fields are filtered out. Nested relation objects
// (populated by the adapter via ?include=) are merged as-is.
// LocaleString fields are rendered according to their effective LocaleMode:
//   - split (default): field holds the resolved string; field+"_i18n" holds the full map
//   - resolve: field always holds the resolved string
//   - dynamic: field holds a string when ctx.Locale is set, the full map otherwise
func (s *defaultSteps) toJSONMap(dbMap map[string]any, model *ModelMeta, ctx *ServerContext) map[string]any {
	if dbMap == nil {
		return nil
	}
	out := make(map[string]any, len(dbMap))

	splitSuffix := "_i18n"
	if ctx != nil && ctx.SplitSuffix != "" {
		splitSuffix = ctx.SplitSuffix
	}

	redacted := ctx != nil && len(ctx.redactedFields) > 0
	for _, f := range model.Fields {
		if f.Tags.Hidden || f.Tags.WriteOnly {
			continue
		}
		// Declared through ctx.RedactResponseField — a per-request decision the
		// static tags cannot express (audit MS-11).
		if redacted && ctx.IsFieldRedacted(f.Tags.JSONName) {
			continue
		}
		v, ok := dbMap[f.Tags.DBName]
		if !ok {
			continue
		}
		if !f.Tags.Locale {
			out[f.Tags.JSONName] = cast(v, f.Type)
			continue
		}

		m, folded := localeValueToMap(v)
		if v != nil && ctx != nil {
			switch {
			case m == nil:
				ctx.Logger().Warn("locale field has unparseable DB value",
					slog.String("model", model.Name),
					slog.String("field", f.Tags.DBName))
			case folded:
				// Readable, but the column is corrupt: it holds a bare scalar,
				// not a locale object. Surfaced rather than silently normalised.
				ctx.Logger().Warn("locale field held a bare scalar, folded to a single locale",
					slog.String("model", model.Name),
					slog.String("field", f.Tags.DBName))
			}
		}
		mode := effectiveLocaleMode(&f, model, ctx)

		switch mode {
		case LocaleModeDynamic:
			if ctx != nil && ctx.Locale != "" {
				chain := effectiveLocaleChain(ctx, &f, model)
				out[f.Tags.JSONName] = resolveLocaleString(m, chain)
			} else {
				// Return full map as map[string]any
				out[f.Tags.JSONName] = localeMapToAny(m)
			}
		case LocaleModeResolve:
			chain := effectiveLocaleChain(ctx, &f, model)
			out[f.Tags.JSONName] = resolveLocaleString(m, chain)
		default: // LocaleModeSplit
			chain := effectiveLocaleChain(ctx, &f, model)
			out[f.Tags.JSONName] = resolveLocaleString(m, chain)
			out[f.Tags.JSONName+splitSuffix] = localeMapToAny(m)
		}
	}

	// Include nested relation objects the adapter may have embedded.
	for _, rel := range model.Relations {
		if nested, ok := dbMap[rel.RelationKey]; ok {
			if relModel, ok := s.reg.Get(rel.RelatedModel); ok {
				out[rel.RelationKey] = s.getNestedField(nested, relModel, ctx)
			}
		}
	}

	// Preserve framework-reserved underscore-prefixed keys (e.g. "_through").
	for k, v := range dbMap {
		if len(k) > 0 && k[0] == '_' {
			out[k] = v
		}
	}

	return out
}

// localeMapToAny converts a map[string]string to map[string]any for JSON output.
// Returns nil when m is nil.
func localeMapToAny(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// enrichLocaleQueryParams post-processes parsed sorts and flat locale-field filters
// to inject locale JSON-path targeting based on the effective locale and mode.
// Called in the Deserialize step after ParseQueryParams, when ctx already has
// Locale/DefaultLocale/DefaultLocaleMode set by LocaleResolver.
func enrichLocaleQueryParams(q *QueryParams, ctx *ServerContext, model *ModelMeta) {
	if q == nil || model == nil {
		return
	}
	for i := range q.Sorts {
		s := &q.Sorts[i]
		if s.IsNested {
			continue // relation sort — column lives on the joined table, not a locale field here
		}
		field := model.FieldByDBName(s.DBName)
		if field == nil || !field.Tags.Locale {
			continue
		}
		mode := effectiveLocaleMode(field, model, ctx)
		if mode == LocaleModeDynamic && (ctx == nil || ctx.Locale == "") {
			continue // dynamic + no explicit locale — sort on raw JSON column
		}
		chain := effectiveLocaleChain(ctx, field, model)
		if len(chain) > 0 {
			s.IsLocale = true
			s.LocaleKey = chain[0]
		}
	}
	for _, f := range q.Filters {
		if f.IsLocale || f.IsNested {
			continue // already has locale targeting or is a relation filter
		}
		field := model.FieldByDBName(f.Field)
		if field == nil || !field.Tags.Locale {
			continue
		}
		mode := effectiveLocaleMode(field, model, ctx)
		localeSet := ctx != nil && ctx.Locale != ""
		if mode == LocaleModeDynamic && !localeSet {
			continue // dynamic + no explicit locale — leave flat filter unchanged
		}
		chain := effectiveLocaleChain(ctx, field, model)
		if len(chain) > 0 {
			f.IsLocale = true
			f.LocaleKey = chain[0]
		}
	}
}

// rewriteFileACL replaces storage keys in row with URLs for fields whose
// mfx:"file_acl" is signed or public. Called after toJSONMap so it operates
// on JSON-keyed output. Errors from URL() are logged and skipped — a missing
// URL is better than a 500 that hides all other fields.
func (s *defaultSteps) rewriteFileACL(goCtx context.Context, model *ModelMeta, row map[string]any) {
	if s.storage == nil {
		return
	}
	for _, f := range model.Fields {
		if !f.Tags.File {
			continue
		}
		acl := f.Tags.FileACL
		if acl == FileACLPrivate || acl == "" {
			continue
		}
		ttl := s.signedURLTTL
		if ttl == 0 {
			ttl = DefaultFileSignedURLTTL
		}
		if acl == FileACLPublic {
			ttl = 0
		}

		// A FileKeys column rewrites every key in place, so the array a client
		// reads is URLs throughout rather than URLs-or-keys depending on shape.
		if f.Type == fileKeysType {
			s.rewriteFileACLList(goCtx, f, row, ttl)
			continue
		}

		key, _ := row[f.Tags.JSONName].(string)
		if key == "" {
			continue
		}
		u, err := s.storage.URL(goCtx, key, ttl)
		if err != nil {
			// Log and leave the raw key — URL rewrite is best-effort.
			continue
		}
		row[f.Tags.JSONName] = u
	}
}

// rewriteFileACLList rewrites each key of a FileKeys column to a URL.
//
// Best-effort per key, matching the single-key path: a key whose URL cannot be
// built stays a raw key rather than dropping out of the array, so the array
// keeps its length and its order either way.
func (s *defaultSteps) rewriteFileACLList(goCtx context.Context, f FieldMeta, row map[string]any, ttl time.Duration) {
	keys, ok := toFileKeys(row[f.Tags.JSONName])
	if !ok || len(keys) == 0 {
		return
	}
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = key
		if key == "" {
			continue
		}
		if u, err := s.storage.URL(goCtx, key, ttl); err == nil {
			out[i] = u
		}
	}
	row[f.Tags.JSONName] = out
}

// cast coerces one driver-supplied cell to the Go kind its model field declares.
// It runs per field per row of every read, so it dispatches on the value with a
// type assertion rather than reflect.TypeOf(value).Kind(): the assertion compares
// the interface's type word, while TypeOf forces the value through the reflect
// machinery for the same answer (PERF-4). _type is already a reflect.Type, so
// Kind() on it is just a field read.
//
// The assertions also retire a latent panic. `reflect.TypeOf(v).Kind() == String`
// is true for a *named* string type (type Code string), but the `v.(string)` that
// followed it only accepts exactly string — so such a value panicked instead of
// being formatted. It now falls through to the same formatting path as any other
// non-string.
func cast(value any, _type reflect.Type) any {
	if value == nil {
		return nil
	}
	switch _type.Kind() {
	case reflect.String:
		return castString(value)
	case reflect.Bool:
		return castBool(value)
	default:
		return value
	}
}

// castString renders a cell into the string its field declares. Driver values that
// are already strings pass straight through; everything else (notably []byte, which
// is how several drivers hand back text) is formatted.
func castString(value any) any {
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprintf("%s", value)
}

// castBool coerces a cell into a bool. SQLite drivers can surface a boolean column
// as a number or a numeric string, so both are accepted.
func castBool(value any) any {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return looseParseBool(v)
	}
	return isNumberAndGreaterThanZero(value)
}

// looseParseBool reads a boolean out of a string column.
//
// Roadmap §11B.10 / checkpoint H10: parse with strconv so "false" / "0" / "no" do
// not silently coerce to true — pre-fix every non-empty string evaluated to true.
func looseParseBool(s string) bool {
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	// strconv.ParseBool accepts only canonical values; for the rest (e.g. "no",
	// "off") treat anything case-insensitively "false"-looking as false and the
	// rest as true, preserving the loose semantics callers may rely on.
	switch strings.ToLower(s) {
	case "", "no", "off", "n":
		return false
	}
	return true
}

func isNumberAndGreaterThanZero(value any) bool {
	v := reflect.ValueOf(value)
	var f float64 = -1

	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f = float64(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		f = float64(v.Uint())
	case reflect.Float32, reflect.Float64:
		f = v.Float()
	default:
		return false // Not a numeric type we can compare
	}

	return f > 0
}

func (s *defaultSteps) getNestedField(nested any, relModel *ModelMeta, ctx *ServerContext) any {
	switch t := nested.(type) {
	case map[string]any:
		s.decryptNested(t, relModel, ctx)
		return s.toJSONMap(t, relModel, ctx)
	case []map[string]any:
		arr := []map[string]any{}
		for _, relObj := range t {
			s.decryptNested(relObj, relModel, ctx)
			arr = append(arr, s.toJSONMap(relObj, relModel, ctx))
		}
		return arr
	default:
		return nested
	}
}

// decryptNested decrypts an included relation's row in place.
//
// The top-level record is decrypted by the DB step, but a row pulled in by
// ?include= reached the serializer straight from the adapter: toJSONMap strips
// hidden and write-only columns but has never decrypted anything, so an
// mfx:"encrypted" field on an included model came back as base64 ciphertext
// (audit MS-L4). Reading the same model directly returned plaintext, so the
// value a client saw depended on how it was fetched.
//
// A failure leaves the row as-is and logs. The alternative — failing the whole
// request because one included relation could not be decrypted — turns a
// degraded field into a dead endpoint, and the top-level record is unaffected.
func (s *defaultSteps) decryptNested(row map[string]any, relModel *ModelMeta, ctx *ServerContext) {
	if row == nil || relModel == nil || !relModel.HasEncryptedFields() || ctx == nil {
		return
	}
	if err := decryptForRead(ctx.Ctx, s.keyProvider, relModel, row); err != nil {
		ctx.Logger().Warn("could not decrypt included relation",
			slog.String("model", relModel.Name),
			slog.String("error", err.Error()))
	}
}

// toFloat64 tries to coerce an interface value to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
