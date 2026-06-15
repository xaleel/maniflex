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
		if out[i], err = typedOf[T](it); err != nil {
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
	var v any
	if tx != nil {
		v, err = tx.FindByID(ctx.Ctx, meta, id, &QueryParams{})
	} else {
		v, err = a.FindByID(ctx.Ctx, meta, id, &QueryParams{})
	}
	if err != nil {
		return nil, err
	}
	return typedOf[T](v)
}

// Create inserts record (a *T) and returns the stored representation, scanned
// back into a field-populated *T.
func Create[T any](ctx *ServerContext, record *T) (*T, error) {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return nil, err
	}
	var v any
	if tx != nil {
		v, err = tx.Create(ctx.Ctx, meta, record)
	} else {
		v, err = a.Create(ctx.Ctx, meta, record)
	}
	if err != nil {
		return nil, err
	}
	// The write path returns a bridge record; re-read via the scanStruct path so
	// the caller gets fully-populated struct fields.
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
	present := make(map[string]struct{}, len(meta.Fields))
	for i := range meta.Fields {
		if col := meta.Fields[i].Tags.DBName; col != "id" {
			present[col] = struct{}{}
		}
	}
	if tx != nil {
		_, err = tx.Update(ctx.Ctx, meta, id, record, present)
	} else {
		_, err = a.Update(ctx.Ctx, meta, id, record, present)
	}
	if err != nil {
		return nil, err
	}
	// Re-read via the scanStruct path for a fully-populated result.
	return Read[T](ctx, id)
}

// Delete removes (or soft-deletes) the T identified by id.
func Delete[T any](ctx *ServerContext, id string) error {
	meta, a, tx, err := modelExec[T](ctx)
	if err != nil {
		return err
	}
	if tx != nil {
		return tx.Delete(ctx.Ctx, meta, id)
	}
	return a.Delete(ctx.Ctx, meta, id)
}
