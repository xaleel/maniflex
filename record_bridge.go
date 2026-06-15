package maniflex

// record_bridge.go — TRANSITIONAL map↔*T bridge (typed-models migration,
// Phase 3). The DBAdapter/Tx interface now speaks the type-erased record carrier
// (*T), but the request pipeline is still map-based until Phase 4. These helpers
// move data across that boundary losslessly so the map pipeline keeps working
// unchanged while the adapter is typed underneath.
//
// Design: the *T is a transport envelope here, not yet the data model. A map
// value is placed into a struct field ONLY when it is directly type-assignable;
// every other value (driver-shaped scalars like int64→int, NULLs, JSON-encoded
// locale, framework *_hmac columns, populated ?include= relations) rides on the
// BaseModel `extra` carrier. The present-key set is recorded too. As a result
// recordToMap(mapToRecord(m)) == m exactly — same keys, same value types — so
// no pipeline behavior changes. The genuinely typed scan/write path (scanStruct
// / buildInsert) is exercised by db/sqlcore's adapter_struct_test and goes live
// with the Phase-4 pipeline rewrite, where these helpers are deleted.

import "reflect"

// recordToMap reconstructs the DB-column-keyed map a bridge-built *T carries.
// It emits the columns named by the present set (set by mapToRecord) plus the
// extra carrier; a nil present set (e.g. a genuinely typed *T) falls back to all
// model columns.
func recordToMap(model *ModelMeta, record any) map[string]any {
	if record == nil {
		return nil
	}
	// Synthetic / GoType-less models (history tables, m2m junctions) have no Go
	// struct type, so their "record" is the map itself — pass it through.
	if m, ok := record.(map[string]any); ok {
		return m
	}
	sv := reflect.ValueOf(record).Elem()
	present := PresentColumns(record)
	out := make(map[string]any, len(model.Fields))
	for i := range model.Fields {
		f := &model.Fields[i]
		if present == nil {
			out[f.Tags.DBName] = sv.FieldByIndex(f.Index).Interface()
			continue
		}
		if _, ok := present[f.Tags.DBName]; ok {
			out[f.Tags.DBName] = sv.FieldByIndex(f.Index).Interface()
		}
	}
	for k, v := range ExtraColumns(record) {
		out[k] = v
	}
	return out
}

// mapToRecord wraps a DB-column-keyed map in a *model.GoType. Directly-assignable
// values land in their struct field; everything else (and the set of present
// column names) is preserved on the carrier so recordToMap can rebuild the map
// exactly.
func mapToRecord(model *ModelMeta, m map[string]any) (any, error) {
	// Synthetic / GoType-less models have no struct to build; the map is the
	// record. recordToMap passes it back through unchanged.
	if model.GoType == nil {
		return m, nil
	}
	pv := reflect.New(model.GoType)
	sv := pv.Elem()
	present := make(map[string]struct{}, len(m))
	var extra map[string]any
	for k, v := range m {
		present[k] = struct{}{}
		if f := model.FieldByDBName(k); f != nil && assignField(sv.FieldByIndex(f.Index), v) {
			continue
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	if rm, ok := pv.Interface().(recordMeta); ok {
		rm.mfxSetPresent(present)
		if extra != nil {
			e := rm.mfxExtra()
			for k, v := range extra {
				e[k] = v
			}
		}
	}
	return pv.Interface(), nil
}

// assignField sets f from val only when val's type is DIRECTLY assignable to f's
// type. Anything else — including a value whose type merely matches f's pointer
// element (e.g. time.Time into a *time.Time field) — returns false so the caller
// preserves val verbatim on the extra carrier. This exact-type rule is what
// makes recordToMap(mapToRecord(m)) == m hold for value types as well as keys:
// the bridge never silently changes a map value's Go type (which would, e.g.,
// defeat the adapter's time.Time→RFC3339 normalisation on write).
func assignField(f reflect.Value, val any) bool {
	if val == nil {
		return false
	}
	rv := reflect.ValueOf(val)
	if rv.Type().AssignableTo(f.Type()) {
		f.Set(rv)
		return true
	}
	return false
}

// ExtraColumns returns the framework-internal extra columns staged on a record
// carrier (NULLs, driver-shaped scalars, *_hmac, populated includes), or nil.
// DB adapters call it from their write builders to append these columns.
// Transitional Phase-3 bridge; removed in Phase 7.
func ExtraColumns(record any) map[string]any {
	if rm, ok := record.(interface{ mfxExtraPeek() map[string]any }); ok {
		return rm.mfxExtraPeek()
	}
	return nil
}

// PresentColumns returns the set of DB column names the bridge recorded as
// present on a record carrier, or nil when none were recorded.
// Transitional Phase-3 bridge; removed in Phase 7.
func PresentColumns(record any) map[string]struct{} {
	if rm, ok := record.(recordMeta); ok {
		return rm.mfxPresent()
	}
	return nil
}

// SetPresentColumns records, on a record carrier, the set of DB columns that
// were actually populated (e.g. the columns scanStruct read for a ?select=
// projection). The serializer emits only present columns. No-op for records
// without a BaseModel carrier.
func SetPresentColumns(record any, cols []string) {
	if rm, ok := record.(recordMeta); ok {
		set := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			set[c] = struct{}{}
		}
		rm.mfxSetPresent(set)
	}
}

// SetExtra merges columns into a record carrier's extra map (populated includes,
// computed fields, _through). No-op for records without a BaseModel carrier.
func SetExtra(record any, m map[string]any) {
	if len(m) == 0 {
		return
	}
	if rm, ok := record.(recordMeta); ok {
		e := rm.mfxExtra()
		for k, v := range m {
			e[k] = v
		}
	}
}

// RecordToMap is the exported transition bridge used by DB adapters to convert a
// returned *T back into the map the still-map pipeline consumes.
// Transitional Phase-3 bridge; removed in Phase 7.
func RecordToMap(model *ModelMeta, record any) map[string]any { return recordToMap(model, record) }

// MapToRecord is the exported transition bridge used by DB adapters to wrap a
// DB-column-keyed map in a *T carrier.
// Transitional Phase-3 bridge; removed in Phase 7.
func MapToRecord(model *ModelMeta, m map[string]any) (any, error) { return mapToRecord(model, m) }

// presentDBKeys returns the set of column names in a DB-column-keyed map — the
// PATCH "present" set the typed Update interface expects during the transition.
func presentDBKeys(m map[string]any) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}
