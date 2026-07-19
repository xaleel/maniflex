package maniflex

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// maxComputedConcurrency bounds how many rows of one page resolve their
// per-row computed fields at once.
//
// It used to be unbounded: applyComputedList spawned one goroutine per row, so
// a 100-row page of a model with a DB-backed computed field fired 100 concurrent
// round-trips, and the load scaled as page-size × concurrent-requests with
// nothing in the middle. The parallelism is still worth having — it is what stops
// one slow resolver serialising a page — but it needs a ceiling.
//
// This is a floor on the fix, not the answer: a resolver that touches a database
// per row is an N+1 whatever its fan-out. Use AddBatchComputedField, which
// resolves the whole page in one call.
const maxComputedConcurrency = 8

// computedExportChunk is how many rows an export resolves batch computed fields
// for at a time. The export writer consumes rows through a lazy iterator so it
// never holds a second copy of the page (v0.2.2); resolving in chunks keeps a
// batch field's extra memory proportional to the chunk rather than the export.
const computedExportChunk = 500

// ComputedFunc derives a virtual field value from one loaded row. It runs in
// the Response step after the DB row has been converted to JSON keys, so the
// `row` map keys are JSON field names.
//
// A non-nil error is logged and the field is omitted from that row — a
// computed-field failure must not poison the whole record. A panic is contained
// the same way: logged with its stack and the field omitted, never propagated
// (audit MS-6). Rows of a list resolve in worker goroutines, where an escaping
// panic would take the process down rather than the request.
//
// The callback receives the *ServerContext, so it can reach ctx.Tx,
// ctx.GetModel and ctx.Auth. Note that rows of a list resolve concurrently
// (bounded by maxComputedConcurrency): work through ctx.Tx is serialised by the
// transaction's single connection, so a per-row resolver that queries is an N+1
// regardless. Prefer BatchComputedFunc for anything that touches a database.
type ComputedFunc func(ctx *ServerContext, row map[string]any) (any, error)

// BatchComputedFunc derives a virtual field for a whole page in one call. It
// receives every row being returned and must return exactly one value per row,
// positionally aligned to `rows` — a length mismatch is logged and the field is
// omitted rather than misaligned onto the wrong records.
//
// This is the answer to the N+1 that ComputedFunc invites: one query resolves a
// column for the whole page.
//
//	srv.AddBatchComputedField("StoreSite", "item_count",
//	    func(ctx *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
//	        ids := make([]any, len(rows))
//	        for i, r := range rows { ids[i] = r["id"] }
//	        counts, err := itemCountsBySite(ctx, ids) // ONE query
//	        if err != nil { return nil, err }
//	        out := make([]any, len(rows))
//	        for i, r := range rows { out[i] = counts[r["id"].(string)] }
//	        return out, nil
//	    })
//
// A single read and the create/update echo call it with a one-row slice, so one
// registration serves every read path.
type BatchComputedFunc func(ctx *ServerContext, rows []map[string]any) ([]any, error)

// ComputedField is one registered virtual field on a model. Name is the JSON
// key it appears under in responses; collisions with real model fields are
// rejected at registration. Computed fields cannot be filtered or sorted —
// they're materialised only on the way out.
type ComputedField struct {
	Name string
	Fn   ComputedFunc
	// Schema is the OpenAPI schema emitted for this field in the model's
	// response schema. Nil means "any type" — the framework cannot infer it,
	// since the callbacks return `any`. Set it with ComputedSchema.
	Schema *OASSchema
	// batchFn resolves the whole page at once. When non-nil it takes precedence
	// over Fn.
	batchFn BatchComputedFunc
	// recordFn is the typed path (set by the generic AddComputedField[T]): it
	// receives the loaded record (the *T from the read/list path, or the result
	// map on create/update echo) instead of the JSON response row. When non-nil
	// it takes precedence over Fn.
	recordFn func(ctx *ServerContext, record any) (any, error)
	// batchRecordFn is the typed batch path (set by AddBatchComputedField[T]).
	// It takes precedence over every other callback.
	batchRecordFn func(ctx *ServerContext, records []any) ([]any, error)
}

// isBatch reports whether this field resolves a whole page in one call.
func (c ComputedField) isBatch() bool {
	return c.batchFn != nil || c.batchRecordFn != nil
}

// ComputedOption configures a computed field at registration.
type ComputedOption func(*ComputedField)

// ComputedSchema declares the OpenAPI schema for a computed field's value.
// Without it the field is still emitted in the spec (read-only), but with no
// type — the framework has no way to know one, since the callbacks return `any`,
// and guessing would put a claim in the spec that nothing enforces.
//
//	srv.AddBatchComputedField("StoreSite", "item_count", fn,
//	    maniflex.ComputedSchema(&maniflex.OASSchema{Type: "integer"}))
func ComputedSchema(s *OASSchema) ComputedOption {
	return func(c *ComputedField) { c.Schema = s }
}

// AddComputedField registers a derived field on the named model. The field
// appears in every read response (single read, create/update echo, list rows)
// for that model. Returns an error when the model is not registered or the
// field name collides with an existing real field or another computed field.
//
//	server.AddComputedField("Product", "stock_level",
//	    func(ctx *maniflex.ServerContext, row map[string]any) (any, error) {
//	        return stockService.CurrentLevel(ctx.Ctx, row["id"].(string))
//	    })
//
// For anything that queries a database, prefer AddBatchComputedField — this
// callback runs once per row.
func (s *Server) AddComputedField(modelName, name string, fn ComputedFunc, opts ...ComputedOption) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddComputedField requires a non-nil function")
	}
	return s.registerComputed(modelName, ComputedField{Name: name, Fn: fn}, opts)
}

// AddBatchComputedField registers a derived field resolved for a whole page in
// one call. The callback receives every row being returned and must return one
// value per row, positionally aligned.
//
//	server.AddBatchComputedField("StoreSite", "item_count", fn,
//	    maniflex.ComputedSchema(&maniflex.OASSchema{Type: "integer"}))
//
// A single read and the create/update echo call it with a one-row slice; an
// export calls it once per chunk of rows.
func (s *Server) AddBatchComputedField(modelName, name string, fn BatchComputedFunc, opts ...ComputedOption) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddBatchComputedField requires a non-nil function")
	}
	return s.registerComputed(modelName, ComputedField{Name: name, batchFn: fn}, opts)
}

// MustAddComputedField is the panic-on-error variant, intended for use in
// `main` or package initialisation.
func (s *Server) MustAddComputedField(modelName, name string, fn ComputedFunc, opts ...ComputedOption) {
	if err := s.AddComputedField(modelName, name, fn, opts...); err != nil {
		panic(err)
	}
}

// MustAddBatchComputedField is the panic-on-error variant of
// AddBatchComputedField.
func (s *Server) MustAddBatchComputedField(modelName, name string, fn BatchComputedFunc, opts ...ComputedOption) {
	if err := s.AddBatchComputedField(modelName, name, fn, opts...); err != nil {
		panic(err)
	}
}

// registerComputed validates and appends a computed field to a model.
func (s *Server) registerComputed(modelName string, cf ComputedField, opts []ComputedOption) error {
	if cf.Name == "" {
		return fmt.Errorf("maniflex: AddComputedField requires a non-empty name")
	}
	for _, o := range opts {
		o(&cf)
	}
	meta, ok := s.registry.Get(modelName)
	if !ok {
		return fmt.Errorf("maniflex: model %q is not registered", modelName)
	}
	if meta.FieldByJSONName(cf.Name) != nil {
		return fmt.Errorf(
			"maniflex: computed field %q on model %q collides with an existing field",
			cf.Name, modelName)
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	for _, c := range meta.Computed {
		if c.Name == cf.Name {
			return fmt.Errorf(
				"maniflex: computed field %q is already registered on model %q",
				cf.Name, modelName)
		}
	}
	meta.Computed = append(meta.Computed, cf)
	return nil
}

// AddComputedField registers a typed derived field: the callback receives the
// loaded record as a *T instead of a JSON map. It's the typed counterpart of
// (*Server).AddComputedField (typed-models migration, T4.5 / locked assumption
// §5). On read and list the record is the scanned *T; on create/update echo it
// is bridged best-effort from the stored row.
//
//	maniflex.AddComputedField(srv, "Product", "stock_level",
//	    func(ctx *maniflex.ServerContext, p *Product) (any, error) {
//	        return stockService.CurrentLevel(ctx.Ctx, p.ID)
//	    })
func AddComputedField[T any](s *Server, modelName, name string, fn func(ctx *ServerContext, record *T) (any, error), opts ...ComputedOption) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddComputedField requires a non-nil function")
	}
	wrapped := func(ctx *ServerContext, record any) (any, error) {
		return fn(ctx, typedRecord[T](ctx, record))
	}
	return s.registerComputed(modelName, ComputedField{Name: name, recordFn: wrapped}, opts)
}

// AddBatchComputedField registers a typed batch-resolved derived field: the
// callback receives the whole page as []*T and returns one value per record,
// positionally aligned.
//
//	maniflex.AddBatchComputedField(srv, "StoreSite", "item_count",
//	    func(ctx *maniflex.ServerContext, sites []*StoreSite) ([]any, error) {
//	        counts, err := itemCountsBySite(ctx, ids(sites)) // ONE query
//	        ...
//	    })
func AddBatchComputedField[T any](s *Server, modelName, name string, fn func(ctx *ServerContext, records []*T) ([]any, error), opts ...ComputedOption) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddBatchComputedField requires a non-nil function")
	}
	wrapped := func(ctx *ServerContext, records []any) ([]any, error) {
		typed := make([]*T, len(records))
		for i, r := range records {
			typed[i] = typedRecord[T](ctx, r)
		}
		return fn(ctx, typed)
	}
	return s.registerComputed(modelName, ComputedField{Name: name, batchRecordFn: wrapped}, opts)
}

// typedRecord coerces a loaded record to *T. The create/update echo carries the
// stored row as a map rather than the scanned record, so it is re-read through
// the typed path to give the callback populated fields. Never returns nil.
//
// That re-read is a second SELECT of the row the write just returned, on every
// create and update of a model with a typed computed field (audit MS-L14).
// Bridging the map with mapToRecord instead was tried and reverted: the bridge
// assigns only values whose Go type already matches the field, and a stored row
// carries driver-shaped scalars (int64 for an int column), which land in the
// extra carrier rather than the struct — so the callback received a zero-valued
// record. scanStruct, which Read[T] goes through, is what does the conversion.
// Removing the round trip needs that conversion reachable from here, not a
// cheaper bridge.
func typedRecord[T any](ctx *ServerContext, record any) *T {
	if rec, ok := record.(*T); ok && rec != nil {
		return rec
	}
	if m, isMap := record.(map[string]any); isMap {
		if id, _ := m["id"].(string); id != "" {
			if r, err := Read[T](ctx, id); err == nil {
				return r
			}
		}
	}
	return new(T)
}

// ── resolution ────────────────────────────────────────────────────────────────

// applyComputed resolves every computed field for one row. Batch fields are
// called with a one-row slice, so a single read and the create/update echo need
// no separate registration.
func applyComputed(ctx *ServerContext, model *ModelMeta, record any, row map[string]any) {
	applyComputedRows(ctx, model, []map[string]any{row}, []any{record})
}

// applyComputedList resolves computed fields across a page of response rows,
// with records[i] the loaded record for rows[i].
func applyComputedList(ctx *ServerContext, model *ModelMeta, rows []any, records []any) {
	maps := make([]map[string]any, 0, len(rows))
	recs := make([]any, 0, len(rows))
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		maps = append(maps, m)
		recs = append(recs, recordAt(records, i))
	}
	applyComputedRows(ctx, model, maps, recs)
}

// applyComputedRows is the one resolution path. rows and records are aligned;
// batch fields resolve in a single call, per-row fields fan out across a
// bounded worker pool.
func applyComputedRows(ctx *ServerContext, model *ModelMeta, rows []map[string]any, records []any) {
	model.mu.RLock()
	fields := model.Computed
	model.mu.RUnlock()
	if len(fields) == 0 || len(rows) == 0 {
		return
	}

	var perRow []ComputedField
	for _, c := range fields {
		if c.isBatch() {
			applyBatchComputed(ctx, model, c, rows, records)
			continue
		}
		perRow = append(perRow, c)
	}
	if len(perRow) > 0 {
		applyPerRowComputed(ctx, model, perRow, rows, records)
	}
}

// applyBatchComputed resolves one batch field for every row in a single call.
func applyBatchComputed(ctx *ServerContext, model *ModelMeta, c ComputedField, rows []map[string]any, records []any) {
	vals, err := callBatchComputed(ctx, model, c, rows, records)
	if err != nil {
		ctx.Logger().Warn("batch computed field failed",
			"model", model.Name, "field", c.Name, "error", err.Error())
		return
	}
	// One value per row, positionally. A short or long slice would silently
	// write values onto the wrong records, so refuse the whole field instead —
	// an absent column is diagnosable; a misaligned one is not.
	if len(vals) != len(rows) {
		ctx.Logger().Warn("batch computed field returned the wrong number of values — field omitted",
			"model", model.Name, "field", c.Name,
			"rows", len(rows), "values", len(vals))
		return
	}
	for i, v := range vals {
		rows[i][c.Name] = v
	}
}

// callBatchComputed invokes one batch callback and converts a panic into an
// error, so the field is logged and omitted exactly as a returned error already
// is (audit MS-6).
//
// A batch callback runs inline, so a panic here was never fatal the way the
// per-row one was — PanicRecoverer caught it and answered 500. But that made
// panic and error disagree within one function, and made the blast radius depend
// on which registration form the field happened to use: converting a per-row
// field to a batch one silently upgraded a bad row from an omitted column to a
// failed request. Containment is the contract; this makes it hold either way.
func callBatchComputed(ctx *ServerContext, model *ModelMeta, c ComputedField, rows []map[string]any, records []any) (vals []any, err error) {
	defer func() {
		if r := recover(); r != nil {
			vals, err = nil, fmt.Errorf("batch computed field %q panicked: %v", c.Name, r)
			ctx.Logger().Error("batch computed field panicked",
				"model", model.Name, "field", c.Name, "panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if c.batchRecordFn != nil {
		return c.batchRecordFn(ctx, records)
	}
	return c.batchFn(ctx, rows)
}

// applyPerRowComputed runs the per-row callbacks over rows, bounded to
// maxComputedConcurrency goroutines rather than one per row.
func applyPerRowComputed(ctx *ServerContext, model *ModelMeta, fields []ComputedField, rows []map[string]any, records []any) {
	if len(rows) == 1 {
		computeRow(ctx, model, fields, recordAt(records, 0), rows[0])
		return
	}

	workers := min(maxComputedConcurrency, len(rows))
	idx := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range idx {
				computeRow(ctx, model, fields, recordAt(records, i), rows[i])
			}
		}()
	}
	for i := range rows {
		idx <- i
	}
	close(idx)
	wg.Wait()
}

// computeRow runs every per-row callback for a single row. Errors are logged
// but do not abort the response — a single bad function must not blank the
// whole record.
func computeRow(ctx *ServerContext, model *ModelMeta, fields []ComputedField, record any, row map[string]any) {
	for _, c := range fields {
		v, err := callComputed(ctx, model, c, record, row)
		if err != nil {
			ctx.Logger().Warn("computed field failed",
				"model", model.Name, "field", c.Name, "error", err.Error())
			continue
		}
		row[c.Name] = v
	}
}

// callComputed invokes one per-row callback and converts a panic into an error,
// so a panicking computed field costs its own field and nothing else (audit
// MS-6).
//
// Without this a panic here was fatal to the process, not to the request:
// applyPerRowComputed fans rows out across a worker pool, and PanicRecoverer
// only wraps the request goroutine — nothing recovers a panic in a goroutine it
// did not start. The behaviour split on row count, which is what hid it: a
// single-row read runs this inline and recovered into a 500, while the same
// field on a two-row page killed the server. An unchecked `row["id"].(string)`
// on a null id was enough, from any client.
//
// The recover belongs here, around the callback, and not around the worker
// body: a worker unwinding out of its receive loop would leave the sender
// blocked on a channel no one reads, trading a crash for a hang. Recovering per
// callback also matches the error path's `continue` — one bad field does not
// cost the row its other computed fields.
func callComputed(ctx *ServerContext, model *ModelMeta, c ComputedField, record any, row map[string]any) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("computed field %q panicked: %v", c.Name, r)
			ctx.Logger().Error("computed field panicked",
				"model", model.Name, "field", c.Name, "panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
		}
	}()
	if c.recordFn != nil {
		return c.recordFn(ctx, record)
	}
	return c.Fn(ctx, row)
}

// recordAt returns the record aligned to row index i, or nil when the caller
// supplied fewer records than rows.
func recordAt(records []any, i int) any {
	if i < len(records) {
		return records[i]
	}
	return nil
}

// hasComputedFields reports whether model has any computed field registered.
func hasComputedFields(model *ModelMeta) bool {
	model.mu.RLock()
	defer model.mu.RUnlock()
	return len(model.Computed) > 0
}
