package maniflex

import (
	"context"
	"fmt"
	"sync"
)

// ComputedFunc derives a virtual field value from the loaded row. It runs in
// the Response step after the DB row has been converted to JSON keys, so the
// `row` map keys are JSON field names.
//
// A non-nil error is logged and the field is omitted from the response — a
// computed-field failure must not poison the whole record.
type ComputedFunc func(ctx context.Context, row map[string]any) (any, error)

// ComputedField is one registered virtual field on a model. Name is the JSON
// key it appears under in responses; collisions with real model fields are
// rejected at registration. Computed fields cannot be filtered or sorted —
// they're materialised only on the way out.
type ComputedField struct {
	Name string
	Fn   ComputedFunc
	// recordFn is the typed path (set by the generic AddComputedField[T]): it
	// receives the loaded record (the *T from the read/list path, or the result
	// map on create/update echo) instead of the JSON response row. When non-nil
	// it takes precedence over Fn.
	recordFn func(ctx *ServerContext, record any) (any, error)
}

// AddComputedField registers a derived field on the named model. The field
// appears in every read response (single read, create/update echo, list rows)
// for that model. Returns an error when the model is not registered or the
// field name collides with an existing real field or another computed field.
//
//	server.AddComputedField("Product", "stock_level",
//	    func(ctx context.Context, row map[string]any) (any, error) {
//	        return stockService.CurrentLevel(ctx, row["id"].(string))
//	    })
func (s *Server) AddComputedField(modelName, name string, fn ComputedFunc) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddComputedField requires a non-nil function")
	}
	return s.registerComputed(modelName, ComputedField{Name: name, Fn: fn})
}

// registerComputed validates and appends a computed field to a model.
func (s *Server) registerComputed(modelName string, cf ComputedField) error {
	if cf.Name == "" {
		return fmt.Errorf("maniflex: AddComputedField requires a non-empty name")
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
//	    func(ctx context.Context, p *Product) (any, error) {
//	        return stockService.CurrentLevel(ctx, p.ID)
//	    })
func AddComputedField[T any](s *Server, modelName, name string, fn func(ctx context.Context, record *T) (any, error)) error {
	if fn == nil {
		return fmt.Errorf("maniflex: AddComputedField requires a non-nil function")
	}
	wrapped := func(ctx *ServerContext, record any) (any, error) {
		rec, ok := record.(*T)
		if !ok {
			// create/update echo carries the stored row as a map; re-read it via
			// the typed (scanStruct) path so the callback gets populated fields.
			if m, isMap := record.(map[string]any); isMap {
				if id, _ := m["id"].(string); id != "" {
					if r, err := Read[T](ctx, id); err == nil {
						rec = r
					}
				}
			}
		}
		if rec == nil {
			rec = new(T)
		}
		return fn(ctx.Ctx, rec)
	}
	return s.registerComputed(modelName, ComputedField{Name: name, recordFn: wrapped})
}

// MustAddComputedField is the panic-on-error variant, intended for use in
// `main` or package initialisation.
func (s *Server) MustAddComputedField(modelName, name string, fn ComputedFunc) {
	if err := s.AddComputedField(modelName, name, fn); err != nil {
		panic(err)
	}
}

// applyComputed runs every registered computed field and writes the result
// under its JSON name into row. The legacy Fn receives the JSON response row;
// the typed recordFn receives the loaded record. Errors are logged but do not
// abort the response — a single bad function must not blank the whole record.
func applyComputed(ctx *ServerContext, model *ModelMeta, record any, row map[string]any) {
	model.mu.RLock()
	fields := model.Computed
	model.mu.RUnlock()
	for _, c := range fields {
		var v any
		var err error
		if c.recordFn != nil {
			v, err = c.recordFn(ctx, record)
		} else {
			v, err = c.Fn(ctx.Ctx, row)
		}
		if err != nil {
			ctx.Logger().Warn("computed field failed",
				"model", model.Name, "field", c.Name, "error", err.Error())
			continue
		}
		row[c.Name] = v
	}
}

// applyComputedList runs computed fields across a slice of response rows, with
// records[i] the loaded record for rows[i]. Each row is processed in its own
// goroutine when there are 2+ rows so a slow computed-field doesn't serialise
// the whole page.
func applyComputedList(ctx *ServerContext, model *ModelMeta, rows []any, records []any) {
	model.mu.RLock()
	hasComputed := len(model.Computed) > 0
	model.mu.RUnlock()
	if !hasComputed || len(rows) == 0 {
		return
	}
	recordAt := func(i int) any {
		if i < len(records) {
			return records[i]
		}
		return nil
	}
	if len(rows) == 1 {
		if m, ok := rows[0].(map[string]any); ok {
			applyComputed(ctx, model, recordAt(0), m)
		}
		return
	}
	var wg sync.WaitGroup
	wg.Add(len(rows))
	for i := range rows {
		go func(idx int) {
			defer wg.Done()
			m, ok := rows[idx].(map[string]any)
			if !ok {
				return
			}
			applyComputed(ctx, model, recordAt(idx), m)
		}(i)
	}
	wg.Wait()
}
