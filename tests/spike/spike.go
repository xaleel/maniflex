// Package spike is the Phase-0 de-risk spike for the typed-models migration
// (docs/plans/impl-plans/typed-models-impl-plan.md, Phase 0). It is THROWAWAY:
// it proves the reflection struct-scan path beats the map path on allocations
// before the rewrite is committed, then its logic moves into db/sqlcore in
// Phase 2 (T2.1 scan.go / T2.2 write.go). It deliberately lives in the tests
// module so it can open both real drivers; nothing in the core module imports it.
//
// It operates purely on the *exported* maniflex.ModelMeta surface (GoType,
// Fields, FieldMeta.Index, Tags) plus database/sql — no access to maniflex
// internals — which is exactly the contract the production layer will have.
package spike

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"maniflex"

	"github.com/google/uuid"
)

// ── Column plan ────────────────────────────────────────────────────────────────
//
// The cost win comes from scanning each column straight into a typed holder
// (reused across rows) and copying it into a freshly-allocated *T, instead of
// the map path's scan-into-[]any (boxing every cell) followed by toJSONMap
// (boxing every cell again). Holders are picked per driver so SQLite's dynamic
// typing (bool as INTEGER 0/1, TIMESTAMP as TEXT) and Postgres's native types
// both round-trip without a per-row interface allocation.

type scanKind int

const (
	skSkip    scanKind = iota // column has no struct field → throwaway
	skString                  // string field
	skInt                     // int/uint family
	skFloat                   // float32/64
	skBool                    // bool field
	skTime                    // time.Time field
	skLocale                  // maniflex.LocaleString (map stored as JSON TEXT/JSONB)
	skScanner                 // field (or *field) implements sql.Scanner — scan in place
)

type colPlan struct {
	kind   scanKind
	index  []int // FieldMeta.Index into the struct; nil for skSkip
	holder any   // reusable pointer target for rows.Scan (nil for skScanner/skSkip-uses-shared)
	ptr    bool  // field is a pointer to the scalar kind (nullable column)
}

var timeType = reflect.TypeOf(time.Time{})
var localeType = reflect.TypeOf(maniflex.LocaleString(nil))
var scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()

// buildPlan resolves one colPlan per result column. Holders are allocated once
// here and reused for every row, so the per-row scan buffer costs nothing.
func buildPlan(meta *maniflex.ModelMeta, cols []string, drv maniflex.DriverType) []colPlan {
	plan := make([]colPlan, len(cols))
	var throwaway any // shared sink for every unmapped column
	for i, col := range cols {
		f := meta.FieldByDBName(col)
		if f == nil {
			plan[i] = colPlan{kind: skSkip, holder: &throwaway}
			continue
		}
		ft := f.Type
		isPtr := ft.Kind() == reflect.Ptr
		base := ft
		if isPtr {
			base = ft.Elem() // nullable column → operate on the pointed-to type
		}
		switch {
		case base == localeType:
			plan[i] = colPlan{kind: skLocale, index: f.Index, holder: new(sql.NullString), ptr: isPtr}
		case base == timeType:
			plan[i] = colPlan{kind: skTime, index: f.Index, holder: newTimeHolder(drv), ptr: isPtr}
		case ft.Implements(scannerType) || reflect.PointerTo(ft).Implements(scannerType):
			plan[i] = colPlan{kind: skScanner, index: f.Index}
		default:
			switch base.Kind() {
			case reflect.String:
				plan[i] = colPlan{kind: skString, index: f.Index, holder: new(sql.NullString), ptr: isPtr}
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				plan[i] = colPlan{kind: skInt, index: f.Index, holder: new(sql.NullInt64), ptr: isPtr}
			case reflect.Float32, reflect.Float64:
				plan[i] = colPlan{kind: skFloat, index: f.Index, holder: new(sql.NullFloat64), ptr: isPtr}
			case reflect.Bool:
				plan[i] = colPlan{kind: skBool, index: f.Index, holder: newBoolHolder(drv), ptr: isPtr}
			default:
				// Unknown Go kind: fall back to a generic any sink so the scan
				// still succeeds; the field is left zero.
				plan[i] = colPlan{kind: skSkip, holder: &throwaway}
			}
		}
	}
	return plan
}

// newTimeHolder: SQLite returns TIMESTAMP columns as TEXT, Postgres as time.Time.
func newTimeHolder(drv maniflex.DriverType) any {
	if drv == maniflex.SQLite {
		return new(sql.NullString)
	}
	return new(sql.NullTime)
}

// newBoolHolder: SQLite stores bool as INTEGER 0/1, Postgres has a native bool.
func newBoolHolder(drv maniflex.DriverType) any {
	if drv == maniflex.SQLite {
		return new(sql.NullInt64)
	}
	return new(sql.NullBool)
}

// ── Row scanner (T0.1) ──────────────────────────────────────────────────────────

// StructScanner holds the resolved per-column plan and a reusable scan buffer
// for one model+column-set. Production (Phase 2) caches one of these per model;
// here it is built per call. It mirrors the two ways database/sql delivers a
// row — via *sql.Rows (ScanRows) or as already-scanned driver values
// (ScanValues, used by the benchmark to isolate framework cost from the driver).
type StructScanner struct {
	meta *maniflex.ModelMeta
	plan []colPlan
	dest []any // reusable; skScanner slots are repointed per row
}

// NewStructScanner resolves the column plan once for the given columns + driver.
func NewStructScanner(meta *maniflex.ModelMeta, cols []string, drv maniflex.DriverType) *StructScanner {
	plan := buildPlan(meta, cols, drv)
	dest := make([]any, len(cols))
	for i, p := range plan {
		if p.kind != skScanner {
			dest[i] = p.holder
		}
	}
	return &StructScanner{meta: meta, plan: plan, dest: dest}
}

// scanOne allocates a fresh *T, points skScanner slots at its fields, runs
// rows.Scan via the supplied scanFn (either *sql.Rows.Scan or a values shim),
// then copies the holders into the struct. It returns the *T.
func (s *StructScanner) scanOne(scanFn func(dest []any) error) (any, error) {
	pv := reflect.New(s.meta.GoType)
	sv := pv.Elem()
	for i, p := range s.plan {
		if p.kind == skScanner {
			s.dest[i] = sv.FieldByIndex(p.index).Addr().Interface()
		}
	}
	if err := scanFn(s.dest); err != nil {
		return nil, err
	}
	if err := assign(sv, s.plan); err != nil {
		return nil, err
	}
	return pv.Interface(), nil
}

// ScanValues converts one row of already-scanned driver values into *T, driving
// each holder's sql.Scanner exactly as *sql.Rows.Scan would. Used by benchmarks
// to measure the framework's per-row conversion cost without driver overhead.
func (s *StructScanner) ScanValues(raw []any) (any, error) {
	return s.scanOne(func(dest []any) error {
		for i, d := range dest {
			sc, ok := d.(sql.Scanner)
			if !ok { // skSkip throwaway sink
				continue
			}
			if err := sc.Scan(raw[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// ScanStruct mirrors db/sqlcore.scanRows but scans into reflect.New(model.GoType)
// rather than a map[string]any. It returns []any whose elements are *T. The drv
// argument selects driver-specific holders for the awkward types.
func ScanStruct(rows *sql.Rows, meta *maniflex.ModelMeta, drv maniflex.DriverType) ([]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	s := NewStructScanner(meta, cols, drv)
	scanFn := func(dest []any) error { return rows.Scan(dest...) }
	var out []any
	for rows.Next() {
		v, err := s.scanOne(scanFn)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// assign copies the post-Scan holder values into the struct fields.
func assign(sv reflect.Value, plan []colPlan) error {
	for _, p := range plan {
		switch p.kind {
		case skSkip, skScanner:
			// skSkip: discarded. skScanner: already written in place by Scan.
			continue
		}
		f := sv.FieldByIndex(p.index)
		switch p.kind {
		case skString:
			h := p.holder.(*sql.NullString)
			if err := requireValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scalarTarget(f, p.ptr, h.Valid); ok {
				tgt.SetString(h.String)
			}
		case skInt:
			h := p.holder.(*sql.NullInt64)
			if err := requireValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scalarTarget(f, p.ptr, h.Valid); ok {
				if tgt.Kind() >= reflect.Uint && tgt.Kind() <= reflect.Uint64 {
					tgt.SetUint(uint64(h.Int64))
				} else {
					tgt.SetInt(h.Int64)
				}
			}
		case skFloat:
			h := p.holder.(*sql.NullFloat64)
			if err := requireValid(h.Valid, f); err != nil {
				return err
			}
			if tgt, ok := scalarTarget(f, p.ptr, h.Valid); ok {
				tgt.SetFloat(h.Float64)
			}
		case skBool:
			var valid, b bool
			switch h := p.holder.(type) {
			case *sql.NullInt64: // SQLite 0/1
				valid, b = h.Valid, h.Int64 != 0
			case *sql.NullBool: // Postgres native
				valid, b = h.Valid, h.Bool
			}
			if err := requireValid(valid, f); err != nil {
				return err
			}
			if tgt, ok := scalarTarget(f, p.ptr, valid); ok {
				tgt.SetBool(b)
			}
		case skTime:
			tm, valid, err := readTime(p.holder)
			if err != nil {
				return err
			}
			if err := requireValid(valid, f); err != nil {
				return err
			}
			if tgt, ok := scalarTarget(f, p.ptr, valid); ok {
				tgt.Set(reflect.ValueOf(tm))
			}
		case skLocale:
			h := p.holder.(*sql.NullString)
			if !h.Valid || h.String == "" {
				continue
			}
			m := maniflex.LocaleString{}
			if err := json.Unmarshal([]byte(h.String), &m); err != nil {
				return fmt.Errorf("spike: locale field %s: %w", f.Type(), err)
			}
			if tgt, ok := scalarTarget(f, p.ptr, true); ok {
				tgt.Set(reflect.ValueOf(m))
			}
		}
	}
	return nil
}

// scalarTarget returns the reflect.Value a scalar should be written into.
// For a non-pointer field that's the field itself. For a pointer field with a
// non-NULL value it allocates a fresh element, points the field at it, and
// returns the element. A NULL into a pointer field leaves it nil (ok=false).
func scalarTarget(f reflect.Value, ptr, valid bool) (reflect.Value, bool) {
	if !valid {
		return reflect.Value{}, false // requireValid already errored for non-pointers
	}
	if ptr {
		nv := reflect.New(f.Type().Elem())
		f.Set(nv)
		return nv.Elem(), true
	}
	return f, true
}

// requireValid enforces the PoD rule: a NULL column scanned into a non-pointer
// field is an error, not a silent zero or a panic. Pointer fields accept NULL.
func requireValid(valid bool, f reflect.Value) error {
	if valid || f.Kind() == reflect.Ptr {
		return nil
	}
	return fmt.Errorf("spike: NULL scanned into non-pointer field of type %s", f.Type())
}

// readTime resolves a time holder back to a time.Time. SQLite stores timestamps
// as RFC3339 TEXT; Postgres returns a native time.Time.
func readTime(holder any) (time.Time, bool, error) {
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
		return time.Time{}, false, fmt.Errorf("spike: cannot parse timestamp %q", h.String)
	default:
		return time.Time{}, false, fmt.Errorf("spike: unexpected time holder %T", holder)
	}
}

// ScanMap is a verbatim copy of db/sqlcore.scanRows: scan every column into a
// boxed []any, then build a map[string]any (with []byte→string). It exists so
// the benchmark can measure the map path's scan-side allocation against
// ScanStruct under identical rows. This is the cost ScanStruct must beat.
func ScanMap(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── Write builders (T0.2) ───────────────────────────────────────────────────────

// BuildInsert generates an INSERT for the given struct pointer, mirroring the
// map path (toDBMap → injectTimestamps → adapter.Create). It reads fields by
// FieldMeta.Index, generates id when empty, and stamps created_at/updated_at.
// Framework-only columns with no struct field (e.g. *_hmac) are NOT emitted —
// the spike doesn't exercise encryption.
func BuildInsert(meta *maniflex.ModelMeta, ptr any, drv maniflex.DriverType) (string, []any) {
	sv := reflect.ValueOf(ptr).Elem()
	now := time.Now().UTC()

	var cols []string
	var args []any
	for i := range meta.Fields {
		f := &meta.Fields[i]
		col := f.Tags.DBName
		v := sv.FieldByIndex(f.Index).Interface()

		switch col {
		case "id":
			if s, _ := v.(string); s == "" {
				v = uuid.NewString()
			}
		case "created_at", "updated_at":
			v = now
		}
		cols = append(cols, col)
		args = append(args, normalise(v, drv))
	}

	ph := placeholders(len(cols), drv)
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		meta.TableName, quoteCols(cols), strings.Join(ph, ", "))
	return q, args
}

// BuildUpdate generates a PATCH-semantics UPDATE: only columns named in present
// (plus updated_at) are written. present holds DB column names.
func BuildUpdate(meta *maniflex.ModelMeta, ptr any, present map[string]struct{}, drv maniflex.DriverType) (string, []any) {
	sv := reflect.ValueOf(ptr).Elem()
	now := time.Now().UTC()

	var sets []string
	var args []any
	var id any
	n := 0
	for i := range meta.Fields {
		f := &meta.Fields[i]
		col := f.Tags.DBName
		v := sv.FieldByIndex(f.Index).Interface()
		if col == "id" {
			id = v
			continue
		}
		if col == "updated_at" {
			continue // always set last, below
		}
		if _, ok := present[col]; !ok {
			continue
		}
		n++
		sets = append(sets, fmt.Sprintf("%s = %s", quoteCol(col), ph1(n, drv)))
		args = append(args, normalise(v, drv))
	}
	n++
	sets = append(sets, fmt.Sprintf("%s = %s", quoteCol("updated_at"), ph1(n, drv)))
	args = append(args, now)
	n++
	args = append(args, id)

	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s",
		meta.TableName, strings.Join(sets, ", "), quoteCol("id"), ph1(n, drv))
	return q, args
}

// ── shared helpers ──────────────────────────────────────────────────────────────

// normalise matches sqlcore.normalise at the field-read boundary: time→RFC3339
// (SQLite TEXT columns), LocaleString→JSON. driver.Valuer types (money.Amount)
// are passed through untouched for database/sql to handle.
func normalise(v any, drv maniflex.DriverType) any {
	if _, ok := v.(driver.Valuer); ok {
		return v
	}
	switch t := v.(type) {
	case time.Time:
		if drv == maniflex.SQLite {
			return t.UTC().Format(time.RFC3339Nano)
		}
		return t.UTC()
	case maniflex.LocaleString:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	}
	return v
}

func placeholders(n int, drv maniflex.DriverType) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ph1(i+1, drv)
	}
	return out
}

func ph1(n int, drv maniflex.DriverType) string {
	if drv == maniflex.Postgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func quoteCols(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteCol(c)
	}
	return strings.Join(q, ", ")
}

func quoteCol(c string) string { return `"` + c + `"` }
