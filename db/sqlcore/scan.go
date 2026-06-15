package sqlcore

// scan.go — typed row scanner (typed-models migration, Phase 2 / T2.1).
//
// Productionizes the Phase-0 spike (maniflex/tests/spike): scan a result row
// straight into reflect.New(model.GoType) via FieldMeta.Index instead of the
// map[string]any path in scanRows. The per-(model, column-set) column plan is
// cached so the reflection setup (field lookup, type classification) runs once,
// not per call. Holders (the sql.Scanner targets database/sql writes into) are
// allocated per scan call, so concurrent scans of the same model never share
// mutable state — the cached plan is immutable.
//
// These functions are not yet wired into the public Adapter methods; that
// happens in Phase 3. Until then the map path (scanRows) remains in use and the
// struct↔map bridge (bridge.go) lets the two coexist.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
)

type scanKind uint8

const (
	skSkip    scanKind = iota // column has no struct field → discard
	skString                  // string
	skInt                     // int/uint family
	skFloat                   // float32/64
	skBool                    // bool
	skTime                    // time.Time
	skLocale                  // maniflex.LocaleString (JSON TEXT/JSONB)
	skScanner                 // field (or *field) implements sql.Scanner — scan in place
)

// columnStep is one resolved column of the cached plan. It is immutable after
// construction; holders are created fresh per scan from kind + driver.
type columnStep struct {
	kind  scanKind
	index []int // FieldMeta.Index; nil for skSkip
	ptr   bool  // field is a pointer to the scalar kind (nullable column)
}

var (
	scanTimeType    = reflect.TypeOf(time.Time{})
	scanLocaleType  = reflect.TypeOf(maniflex.LocaleString(nil))
	scanScannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)

// scanPlanFor returns the cached column plan for (model, cols), building it on
// first use. The cache key combines the model pointer with the column signature
// so SELECT * and ?select= projections get distinct plans.
func (a *Adapter) scanPlanFor(model *maniflex.ModelMeta, cols []string) []columnStep {
	key := scanPlanKey(model, cols)
	if v, ok := a.scanPlans.Load(key); ok {
		return v.([]columnStep)
	}
	steps := buildColumnPlan(model, cols)
	a.scanPlans.Store(key, steps)
	return steps
}

func scanPlanKey(model *maniflex.ModelMeta, cols []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%p|", model)
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c)
	}
	return b.String()
}

// buildColumnPlan classifies each column against the model's fields. The kind is
// driver-independent; the holder type for the awkward kinds (bool, time) is
// chosen per driver at scan time in newHolder.
func buildColumnPlan(model *maniflex.ModelMeta, cols []string) []columnStep {
	steps := make([]columnStep, len(cols))
	for i, col := range cols {
		f := model.FieldByDBName(col)
		if f == nil {
			steps[i] = columnStep{kind: skSkip}
			continue
		}
		ft := f.Type
		isPtr := ft.Kind() == reflect.Pointer
		base := ft
		if isPtr {
			base = ft.Elem()
		}
		switch {
		case base == scanLocaleType:
			steps[i] = columnStep{kind: skLocale, index: f.Index, ptr: isPtr}
		case base == scanTimeType:
			steps[i] = columnStep{kind: skTime, index: f.Index, ptr: isPtr}
		case ft.Implements(scanScannerType) || reflect.PointerTo(ft).Implements(scanScannerType):
			steps[i] = columnStep{kind: skScanner, index: f.Index}
		default:
			switch base.Kind() {
			case reflect.String:
				steps[i] = columnStep{kind: skString, index: f.Index, ptr: isPtr}
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				steps[i] = columnStep{kind: skInt, index: f.Index, ptr: isPtr}
			case reflect.Float32, reflect.Float64:
				steps[i] = columnStep{kind: skFloat, index: f.Index, ptr: isPtr}
			case reflect.Bool:
				steps[i] = columnStep{kind: skBool, index: f.Index, ptr: isPtr}
			default:
				steps[i] = columnStep{kind: skSkip}
			}
		}
	}
	return steps
}

// newHolder allocates a fresh, driver-appropriate sql.Scanner target for a kind.
// SQLite surfaces bool as INTEGER 0/1 and TIMESTAMP as TEXT; Postgres has native
// bool and time.Time. skScanner has no holder (scanned into the field directly);
// skSkip uses a shared throwaway sink supplied by the caller.
func newHolder(k scanKind, drv maniflex.DriverType) any {
	switch k {
	case skString, skLocale:
		return new(sql.NullString)
	case skInt:
		return new(sql.NullInt64)
	case skFloat:
		return new(sql.NullFloat64)
	case skBool:
		if drv == maniflex.SQLite {
			return new(sql.NullInt64)
		}
		return new(sql.NullBool)
	case skTime:
		if drv == maniflex.SQLite {
			return new(sql.NullString)
		}
		return new(sql.NullTime)
	default:
		return nil
	}
}

// scanStruct scans every row into a *model.GoType using the adapter's cached
// column plan. The txAdapter has its own (uncached) entry point; both share
// scanStructRows.
func (a *Adapter) scanStruct(rows *sql.Rows, model *maniflex.ModelMeta) ([]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	return scanStructRows(rows, model, a.driver, cols, a.scanPlanFor(model, cols))
}

// scanStructRows is the shared row→*T scan loop, parameterised by driver and a
// precomputed column plan (cached by the Adapter, freshly built by txAdapter).
func scanStructRows(rows *sql.Rows, model *maniflex.ModelMeta, driver maniflex.DriverType, cols []string, steps []columnStep) ([]any, error) {
	// Per-call holders + scan-destination slice. Holder pointers are stable
	// across this call's rows; skScanner slots are repointed at each row's field.
	var throwaway any
	holders := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i, st := range steps {
		switch st.kind {
		case skScanner:
			// repointed per row
		case skSkip:
			dest[i] = &throwaway
		default:
			h := newHolder(st.kind, driver)
			holders[i] = h
			dest[i] = h
		}
	}

	var out []any
	for rows.Next() {
		pv := reflect.New(model.GoType)
		sv := pv.Elem()
		for i, st := range steps {
			if st.kind == skScanner {
				dest[i] = sv.FieldByIndex(st.index).Addr().Interface()
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if err := assignStruct(sv, steps, holders); err != nil {
			return nil, err
		}
		rec := pv.Interface()
		// Record which columns were populated so the serializer emits exactly
		// those (honouring ?select= projections, matching the map path).
		maniflex.SetPresentColumns(rec, cols)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// assignStruct copies the post-Scan holder values into the struct fields. The
// skScanner fields are already populated in place by rows.Scan.
func assignStruct(sv reflect.Value, steps []columnStep, holders []any) error {
	for i, st := range steps {
		switch st.kind {
		case skSkip, skScanner:
			continue
		}
		f := sv.FieldByIndex(st.index)
		switch st.kind {
		case skString:
			h := holders[i].(*sql.NullString)
			if err := requireScanValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, h.Valid); ok {
				tgt.SetString(h.String)
			}
		case skInt:
			h := holders[i].(*sql.NullInt64)
			if err := requireScanValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, h.Valid); ok {
				if tgt.Kind() >= reflect.Uint && tgt.Kind() <= reflect.Uint64 {
					tgt.SetUint(uint64(h.Int64))
				} else {
					tgt.SetInt(h.Int64)
				}
			}
		case skFloat:
			h := holders[i].(*sql.NullFloat64)
			if err := requireScanValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, h.Valid); ok {
				tgt.SetFloat(h.Float64)
			}
		case skBool:
			var valid, b bool
			switch h := holders[i].(type) {
			case *sql.NullInt64: // SQLite 0/1
				valid, b = h.Valid, h.Int64 != 0
			case *sql.NullBool: // Postgres native
				valid, b = h.Valid, h.Bool
			}
			if err := requireScanValid(valid, f); err != nil {
				return err
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, valid); ok {
				tgt.SetBool(b)
			}
		case skTime:
			tm, valid, err := readScanTime(holders[i])
			if err != nil {
				return err
			}
			if err := requireScanValid(valid, f); err != nil {
				return err
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, valid); ok {
				tgt.Set(reflect.ValueOf(tm))
			}
		case skLocale:
			h := holders[i].(*sql.NullString)
			if !h.Valid || h.String == "" {
				continue
			}
			m := maniflex.LocaleString{}
			if err := json.Unmarshal([]byte(h.String), &m); err != nil {
				return fmt.Errorf("sqlcore: locale field %s: %w", f.Type(), err)
			}
			if tgt, ok := scanScalarTarget(f, st.ptr, true); ok {
				tgt.Set(reflect.ValueOf(m))
			}
		}
	}
	return nil
}

// scanScalarTarget returns the reflect.Value a scalar should be written into.
// Non-pointer fields write in place; pointer fields with a non-NULL value get a
// fresh element allocated and pointed at. A NULL into a pointer leaves it nil.
func scanScalarTarget(f reflect.Value, ptr, valid bool) (reflect.Value, bool) {
	if !valid {
		return reflect.Value{}, false
	}
	if ptr {
		nv := reflect.New(f.Type().Elem())
		f.Set(nv)
		return nv.Elem(), true
	}
	return f, true
}

// requireScanValid enforces the rule that a NULL column scanned into a
// non-pointer field is a clear error, never a panic. Pointer fields accept NULL.
func requireScanValid(valid bool, f reflect.Value) error {
	if valid || f.Kind() == reflect.Pointer {
		return nil
	}
	return fmt.Errorf("sqlcore: NULL scanned into non-pointer field of type %s", f.Type())
}

// readScanTime resolves a time holder. SQLite stores timestamps as RFC3339 TEXT;
// Postgres returns a native time.Time.
func readScanTime(holder any) (time.Time, bool, error) {
	switch h := holder.(type) {
	case *sql.NullTime:
		return h.Time, h.Valid, nil
	case *sql.NullString:
		if !h.Valid || h.String == "" {
			return time.Time{}, false, nil
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, h.String); err == nil {
				return t.UTC(), true, nil
			}
		}
		return time.Time{}, false, fmt.Errorf("sqlcore: cannot parse timestamp %q", h.String)
	default:
		return time.Time{}, false, fmt.Errorf("sqlcore: unexpected time holder %T", holder)
	}
}

// recordFromDBMap scans a DB-column-keyed map (driver-shaped values, e.g. an
// include's nested row) into a *model.GoType, reusing the column-plan scanner.
// Columns are sorted so the plan cache key is stable across map iteration orders.
func recordFromDBMap(model *maniflex.ModelMeta, driver maniflex.DriverType, m map[string]any) (any, error) {
	cols := make([]string, 0, len(m))
	for k := range m {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	raw := make([]any, len(cols))
	for i, c := range cols {
		raw[i] = m[c]
	}
	return convertRowValues(model, driver, cols, raw, buildColumnPlan(model, cols))
}

// scanStructValues converts one row of already-scanned driver values into a *T,
// driving each holder's sql.Scanner exactly as rows.Scan would. It is the
// DB-free seam the scanner shares with rows.Scan: tests use it to exercise the
// conversion (and the cached plan, concurrently) without a SQL driver, which the
// core module does not depend on.
func (a *Adapter) scanStructValues(model *maniflex.ModelMeta, cols []string, raw []any) (any, error) {
	return convertRowValues(model, a.driver, cols, raw, a.scanPlanFor(model, cols))
}

// convertRowValues is the shared DB-free converter: it drives each holder's
// sql.Scanner with the supplied raw values, then assigns into a fresh *T.
func convertRowValues(model *maniflex.ModelMeta, driver maniflex.DriverType, cols []string, raw []any, steps []columnStep) (any, error) {
	pv := reflect.New(model.GoType)
	sv := pv.Elem()
	holders := make([]any, len(cols))
	for i, st := range steps {
		switch st.kind {
		case skSkip:
			continue
		case skScanner:
			sc := sv.FieldByIndex(st.index).Addr().Interface().(sql.Scanner)
			if err := sc.Scan(raw[i]); err != nil {
				return nil, err
			}
		default:
			h := newHolder(st.kind, driver)
			holders[i] = h
			if err := h.(sql.Scanner).Scan(raw[i]); err != nil {
				return nil, err
			}
		}
	}
	if err := assignStruct(sv, steps, holders); err != nil {
		return nil, err
	}
	return pv.Interface(), nil
}
