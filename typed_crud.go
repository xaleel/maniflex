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
//
// It returns no present map: a create writes every column and needs none, and an
// update builds its own through updatablePresent, which is where the readonly and
// immutable columns are excluded (audit MS-5).
func encryptedWriteRecord[T any](ctx *ServerContext, meta *ModelMeta, record *T) (any, error) {
	data := recordToMap(meta, record)
	if err := encryptForWrite(ctx.Ctx, ctx.keyProvider, meta, data); err != nil {
		return nil, err
	}
	return mapToRecord(meta, data)
}

// createPresent lists the columns a typed Create writes. That is every column,
// minus those left at their Go zero *and* carrying an mfx:"default:" tag.
//
// A default: tag becomes a SQL DEFAULT clause on the column and nothing more —
// there is no Go-side application of it — so a default fires only when the
// INSERT omits the column. The HTTP create path omits whatever the request body
// did not send, and got defaults for free; Create[T] passed no present set at
// all, which the builder reads as "write every column", so the same model
// created through the two doors disagreed: status "active"/priority 5 over
// HTTP, ""/0 in Go (audit MS-13). Pointer fields were no escape — a nil *string
// was written as an explicit NULL, which also beats the DEFAULT.
//
// Only defaulted columns are skipped, not every zero-valued one. Widening it
// would mean a false, an empty string or a 0 stops meaning "this value" on
// every column in every model, so a deliberate zero would silently become NULL
// or some DB default the model never declared. The narrow rule leaves
// full-struct semantics intact everywhere a default was not asked for.
//
// The cost is that a defaulted non-pointer column cannot be given an explicit
// zero — Priority: 0 against default:5 stores 5. Make the field a pointer when
// that distinction matters: nil takes the default, new(int) writes 0.
func createPresent[T any](meta *ModelMeta, record *T) map[string]struct{} {
	rv := reflect.ValueOf(record).Elem()
	present := make(map[string]struct{}, len(meta.Fields))
	for i := range meta.Fields {
		f := &meta.Fields[i]
		if f.Tags.Default != "" && rv.FieldByIndex(f.Index).IsZero() {
			continue
		}
		present[f.Tags.DBName] = struct{}{}
	}
	// No *_hmac companions here, unlike updatablePresent: those are not model
	// fields. encryptForWrite adds them to the map after recordToMap has already
	// filtered by this set, and mapToRecord then rebuilds the present set from
	// the map's own keys.
	return present
}

// swapPresent installs present on a record carrier and returns a func restoring
// the previous value. Create mutates the caller's record rather than a copy so
// that the id buildInsertSQL generates still lands on it, which callers can see.
func swapPresent(record any, present map[string]struct{}) func() {
	rm, ok := record.(recordMeta)
	if !ok {
		return func() {}
	}
	old := rm.mfxPresent()
	rm.mfxSetPresent(present)
	return func() { rm.mfxSetPresent(old) }
}

// updatablePresent lists the columns a typed Update may write: every column
// except id, minus the ones the model marks readonly or immutable.
//
// Update[T] is a full-record write — an omitted field is deliberately blanked,
// which is documented and pinned by test — but "every column except id" also
// swept in created_at. It is mfx:"readonly" and framework-managed, so a caller
// building a fresh struct to change one field stamped the zero time over the
// row's real creation date (audit MS-5). readonly and immutable were enforced
// only by the Validate step, which no typed helper runs, so the tags protected
// the HTTP surface alone and any readonly column was silently zeroed the same
// way. Excluding them here rather than in the adapter's buildUpdateSQL is
// deliberate: that builder is shared with the scheduled sweep, key rotation and
// the jobs mount, which each write a named column that an app may well have
// marked readonly precisely so clients cannot.
//
// To write such a column on purpose, use ctx.GetModel(name).Update with an
// explicit present map — an explicit list is a different statement of intent
// from a struct that happens to carry a zero value.
func updatablePresent(meta *ModelMeta) map[string]struct{} {
	present := make(map[string]struct{}, len(meta.Fields))
	for i := range meta.Fields {
		f := &meta.Fields[i]
		if f.Tags.DBName == "id" || f.Tags.Readonly || f.Tags.Immutable {
			continue
		}
		present[f.Tags.DBName] = struct{}{}
		// The hmac companion is derived from the field's value, so it travels
		// with it — and is skipped with it when the field is not written, since
		// a stale hmac is worse than an absent one.
		if f.Tags.Encrypted {
			present[f.Tags.DBName+"_hmac"] = struct{}{}
		}
	}
	return present
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
func Create[T any](ctx *ServerContext, record *T, opts ...WriteOption) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	if err := stampScope(ctx, meta, record); err != nil {
		return nil, err
	}
	// Declared before the encryption branch so both write paths inherit it:
	// recordToMap (inside encryptedWriteRecord) filters the map by this same set.
	present := createPresent(meta, record)
	if !applyWriteOptions(opts).skipValidation {
		if err := validateRecordValues(meta, record, present); err != nil {
			return nil, err
		}
	}
	defer swapPresent(record, present)()
	// Encrypted models write through the map bridge so mfx:"encrypted" fields are
	// encrypted (and *_hmac companions written) instead of stored as plaintext by
	// the struct fast-path. Unencrypted models keep the direct struct write.
	var write any = record
	if meta.HasEncryptedFields() {
		if write, err = encryptedWriteRecord(ctx, meta, record); err != nil {
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
// update: every writable column is written, so a field left at its zero value
// overwrites the stored one. For partial/PATCH writes use the pipeline (HTTP
// PATCH) or ctx.GetModel(name).Update with a present map.
//
// Not written: id, and any column the model marks mfx:"readonly" or
// mfx:"immutable" — which includes the framework-managed created_at. See
// updatablePresent. updated_at is still stamped by the adapter.
func Update[T any](ctx *ServerContext, id string, record *T, opts ...WriteOption) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	var write any = record
	present := updatablePresent(meta)
	if !applyWriteOptions(opts).skipValidation {
		if err := validateRecordValues(meta, record, present); err != nil {
			return nil, err
		}
	}
	if meta.HasEncryptedFields() {
		if write, err = encryptedWriteRecord(ctx, meta, record); err != nil {
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
