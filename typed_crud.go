package maniflex

// typed_crud.go — typed cross-model CRUD helpers (typed-models migration,
// Phase 5 / T5.2). These generic free functions are the typed counterpart to the
// string-named ctx.GetModel(name) accessor: they resolve the model from the type
// parameter T, route through the active transaction (ctx.Tx) when one is set,
// and exchange concrete *T values instead of map[string]any.
//
//	users, err := maniflex.List[User](ctx, nil)        // []*User
//	u, err := maniflex.Read[User](ctx, id)             // *User
//	created, err := maniflex.Create(ctx, &User{Name: "Jane"})
//
// The string-named ctx.GetModel(name) accessor remains for dynamic/by-name access
// (it is the map shim until Phase 7).

import (
	"fmt"
	"reflect"
	"strings"
)

// decryptTypedResult converts an adapter result (*T) to plaintext when the model
// has mfx:"encrypted" fields, going through the map bridge so the same decrypt
// logic as the HTTP path applies. A no-op passthrough for unencrypted models.
func decryptTypedResult[T any](ctx *ServerContext, meta *ModelMeta, v any) (*T, error) {
	if v == nil {
		return nil, nil
	}
	if !meta.HasEncryptedFields() {
		return typedOf[T](v)
	}
	m := recordToMap(meta, v)
	if err := decryptForRead(ctx.Ctx, ctx.keyProvider, meta, m); err != nil {
		return nil, err
	}
	rec, err := mapToRecord(meta, m)
	if err != nil {
		return nil, err
	}
	return typedOf[T](rec)
}

// encryptedWriteRecord builds a bridge record from a *T for an encrypted model,
// with its mfx:"encrypted" fields encrypted (and {field}_hmac companions filled).
// present receives every written column (including the hmac companions).
func encryptedWriteRecord[T any](ctx *ServerContext, meta *ModelMeta, record *T) (any, map[string]struct{}, error) {
	data := recordToMap(meta, record)
	if err := encryptForWrite(ctx.Ctx, ctx.keyProvider, meta, data); err != nil {
		return nil, nil, err
	}
	present := make(map[string]struct{}, len(meta.Fields)+len(data))
	for i := range meta.Fields {
		if col := meta.Fields[i].Tags.DBName; col != "id" {
			present[col] = struct{}{}
		}
	}
	for k := range data {
		if strings.HasSuffix(k, "_hmac") {
			present[k] = struct{}{}
		}
	}
	rec, err := mapToRecord(meta, data)
	return rec, present, err
}

// modelExec resolves the model meta for T and the adapter/tx to route through,
// mirroring ctx.GetModel's per-model-adapter + same-adapter-tx rules.
func modelExec[T any](ctx *ServerContext) (*ModelMeta, DBAdapter, Tx, error) {
	if ctx == nil || ctx.reg == nil {
		return nil, nil, nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}
	name := reflect.TypeOf(*new(T)).Name()
	meta, ok := ctx.reg.Get(name)
	if !ok {
		return nil, nil, nil, fmt.Errorf("maniflex: model %q is not registered", name)
	}
	target := meta.ResolveAdapter(ctx.adapter)
	tx := ctx.Tx
	if tx != nil && target != ctx.requestAdapter() {
		tx = nil // the request tx belongs to a different adapter; don't cross it
	}
	return meta, target, tx, nil
}

// typedOf asserts an adapter result (always a *T per the DBAdapter contract) to
// the concrete type, returning a clear error on the rare mismatch.
func typedOf[T any](v any) (*T, error) {
	if v == nil {
		return nil, nil
	}
	rec, ok := v.(*T)
	if !ok {
		return nil, fmt.Errorf("maniflex: adapter returned %T, want *%T", v, *new(T))
	}
	return rec, nil
}

// List returns a page of T records. q may be nil (page 1, default limit).
func List[T any](ctx *ServerContext, q *QueryParams) ([]*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &QueryParams{Page: 1, Limit: defaultLimit}
	}
	// A hand-built &QueryParams{} leaves Limit at 0, which would issue LIMIT 0 and
	// return no rows. Clamp to the default so a non-nil query without an explicit
	// limit behaves like the nil case (the HTTP path fills these from ?limit/?page).
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	q = ctx.scopedQuery(q) // ActionScope, when one is in force; a no-op otherwise
	var items []any
	if tx != nil {
		items, _, err = tx.FindMany(ctx.Ctx, meta, q)
	} else {
		items, _, err = a.FindMany(ctx.Ctx, meta, q)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*T, len(items))
	for i, it := range items {
		if out[i], err = decryptTypedResult[T](ctx, meta, it); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Read returns the single T identified by id, or maniflex.ErrNotFound.
func Read[T any](ctx *ServerContext, id string) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	q := ctx.scopedQuery(&QueryParams{})
	var v any
	if tx != nil {
		v, err = tx.FindByID(ctx.Ctx, meta, id, q)
	} else {
		v, err = a.FindByID(ctx.Ctx, meta, id, q)
	}
	if err != nil {
		return nil, err
	}
	return decryptTypedResult[T](ctx, meta, v)
}

// stampScope writes the ActionScope's columns onto a record about to be created.
// Unlike the accessor's map path there is no map to overwrite — the value has to
// be set on the struct field the column resolves to.
func stampScope(ctx *ServerContext, meta *ModelMeta, record any) error {
	inject, err := ctx.actionScope.injectable()
	if err != nil || len(inject) == 0 {
		return err
	}
	rv := reflect.ValueOf(record)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("maniflex: cannot apply %s to a nil record",
			ctx.actionScope.scopeName())
	}
	rv = rv.Elem()
	for col, v := range inject {
		f := meta.FieldByDBName(col)
		if f == nil {
			return fmt.Errorf(
				"maniflex: %s scopes column %q but model %q has no such column — the scope "+
					"cannot be applied to this model",
				ctx.actionScope.scopeName(), col, meta.Name)
		}
		if !assignField(rv.FieldByIndex(f.Index), v) {
			return fmt.Errorf(
				"maniflex: %s scopes column %q with a %T, which does not fit field %q of model %q",
				ctx.actionScope.scopeName(), col, v, f.Name, meta.Name)
		}
	}
	return nil
}

// typedInScope is the write counterpart of the scoped read for the generics:
// Update and Delete are keyed by id alone, so the record is looked up through
// the scope first and a miss is ErrNotFound — the same answer Read gives.
func typedInScope(ctx *ServerContext, meta *ModelMeta, a DBAdapter, tx Tx, id string) error {
	sf := ctx.scopeFilters()
	if len(sf) == 0 {
		return nil
	}
	q := &QueryParams{Page: 1, Limit: 1, Filters: sf}
	var err error
	if tx != nil {
		_, err = tx.FindByID(ctx.Ctx, meta, id, q)
	} else {
		_, err = a.FindByID(ctx.Ctx, meta, id, q)
	}
	return err
}

// Create inserts record (a *T) and returns the stored representation, scanned
// back into a field-populated *T.
//
// Under an ActionScope the scope's columns are stamped onto the record first,
// overwriting whatever the caller set — a row created outside the scope would be
// invisible to the caller that created it, and letting a caller choose the value
// is exactly the placement the scope exists to prevent.
func Create[T any](ctx *ServerContext, record *T) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	if err := stampScope(ctx, meta, record); err != nil {
		return nil, err
	}
	// Encrypted models write through the map bridge so mfx:"encrypted" fields are
	// encrypted (and *_hmac companions written) instead of stored as plaintext by
	// the struct fast-path. Unencrypted models keep the direct struct write.
	var write any = record
	if meta.HasEncryptedFields() {
		if write, _, err = encryptedWriteRecord(ctx, meta, record); err != nil {
			return nil, err
		}
	}
	var v any
	if tx != nil {
		v, err = tx.Create(ctx.Ctx, meta, write)
	} else {
		v, err = a.Create(ctx.Ctx, meta, write)
	}
	if err != nil {
		return nil, err
	}
	// The write path returns a bridge record; re-read via the scanStruct path so
	// the caller gets fully-populated (and decrypted) struct fields.
	return Read[T](ctx, fmt.Sprint(recordToMap(meta, v)["id"]))
}

// Update writes record (a *T) over the row identified by id. It is a full-record
// update: every column (except id) is written. For partial/PATCH writes use the
// pipeline (HTTP PATCH) or ctx.GetModel(name).Update with a present map.
func Update[T any](ctx *ServerContext, id string, record *T) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	var write any = record
	present := make(map[string]struct{}, len(meta.Fields))
	for i := range meta.Fields {
		if col := meta.Fields[i].Tags.DBName; col != "id" {
			present[col] = struct{}{}
		}
	}
	if meta.HasEncryptedFields() {
		if write, present, err = encryptedWriteRecord(ctx, meta, record); err != nil {
			return nil, err
		}
	}
	if err := typedInScope(ctx, meta, a, tx, id); err != nil {
		return nil, err
	}
	if tx != nil {
		_, err = tx.Update(ctx.Ctx, meta, id, write, present)
	} else {
		_, err = a.Update(ctx.Ctx, meta, id, write, present)
	}
	if err != nil {
		return nil, err
	}
	// Re-read via the scanStruct path for a fully-populated (decrypted) result.
	return Read[T](ctx, id)
}

// Delete removes (or soft-deletes) the T identified by id. Under an ActionScope
// a record outside the scope is ErrNotFound and nothing is deleted.
func Delete[T any](ctx *ServerContext, id string) error {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return err
	}
	if err := typedInScope(ctx, meta, a, tx, id); err != nil {
		return err
	}
	if tx != nil {
		return tx.Delete(ctx.Ctx, meta, id)
	}
	return a.Delete(ctx.Ctx, meta, id)
}
