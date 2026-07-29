package maniflex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"time"
)

// CursorParams carries the state for one keyset (cursor) paginated list request.
// It is set on QueryParams.Cursor by ParseQueryParams when the request opts into
// cursor mode via ?cursor= on a model that declares a cursor field
// (mfx:"cursor_field:..." or ModelConfig.CursorField).
//
// Keyset pagination orders by (cursor field, id) so the boundary is total even
// when the cursor field has ties, then walks the dataset with a WHERE bound
// instead of OFFSET — so it never skips or duplicates rows when rows are
// inserted or deleted between page fetches.
//
// The Field/Direction/After* members are inputs filled from the request; the DB
// adapter writes NextCursor/HasMore back as outputs after fetching the page.
type CursorParams struct {
	// Field is the DB column the cursor walks (resolved from the model).
	Field string
	// Direction is the keyset walk direction; the id tiebreaker uses the same
	// direction so the ORDER BY and the boundary predicate stay consistent.
	Direction SortDir

	// HasToken is false on the first page (?cursor= with an empty value) and
	// true once a token from a previous page is supplied. When false the bound
	// predicate is omitted and the walk starts from the first row.
	HasToken   bool
	AfterValue any    // decoded cursor field value of the last row of the previous page
	AfterID    string // decoded id of the last row of the previous page (tiebreaker)

	// NextCursor is the token to fetch the next page, set by the adapter from the
	// last returned row. Empty when the page was the last one (HasMore == false).
	NextCursor string
	// HasMore reports whether at least one more row exists after this page.
	HasMore bool
}

const cursorTokenVersion = 1

const (
	cursorTypeString  = "string"
	cursorTypeInteger = "integer"
	cursorTypeNumber  = "number"
	cursorTypeBoolean = "boolean"
	cursorTypeTime    = "time"
	cursorTypeJSON    = "json"
)

// cursorToken is the wire shape encoded into the opaque ?cursor= string.
// Version and Type are absent on legacy tokens and present on every token
// emitted since v1. A type tag prevents a valid token from one cursor field
// being rebound with different database comparison semantics on another.
type cursorToken struct {
	Version int    `json:"version,omitempty"`
	Type    string `json:"type,omitempty"`
	V       any    `json:"v"`
	ID      string `json:"id"`
}

// EncodeCursor builds the opaque token that points just past (value, id) in the
// keyset ordering. It is base64url(JSON) so it survives in a query string and
// carries no separator-collision risk.
func EncodeCursor(value any, id string) string {
	if id == "" {
		return ""
	}
	value, valueType, ok := encodeCursorValue(value)
	if !ok {
		return ""
	}
	b, err := json.Marshal(cursorToken{
		Version: cursorTokenVersion,
		Type:    valueType,
		V:       value,
		ID:      id,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// encodeCursorValue converts values into an explicitly typed wire
// representation. In particular, time.Time uses the same fixed-width UTC form
// SQLite stores, rather than time.Time's variable-width RFC3339Nano JSON.
func encodeCursorValue(value any) (any, string, bool) {
	switch v := value.(type) {
	case time.Time:
		return CanonicalTime(v), cursorTypeTime, true
	case *time.Time:
		if v != nil {
			return CanonicalTime(*v), cursorTypeTime, true
		}
	case string:
		return v, cursorTypeString, true
	case bool:
		return v, cursorTypeBoolean, true
	case json.Number:
		if _, err := parseCursorInteger(v); err == nil {
			return v, cursorTypeInteger, true
		}
		if f, err := v.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return v, cursorTypeNumber, true
		}
	}
	if value == nil {
		return nil, "", false
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.String:
		return rv.String(), cursorTypeString, true
	case reflect.Bool:
		return rv.Bool(), cursorTypeBoolean, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), cursorTypeInteger, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), cursorTypeInteger, true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, cursorTypeNumber, true
		}
	default:
		return nil, "", false
	}
	return nil, "", false
}

// DecodeCursor reverses EncodeCursor. It accepts legacy unversioned tokens so
// clients can finish an in-progress walk across an upgrade.
func DecodeCursor(token string) (value any, id string, err error) {
	t, value, err := decodeCursorToken(token)
	if err != nil {
		return nil, "", err
	}
	return value, t.ID, nil
}

func decodeCursorToken(token string) (cursorToken, any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursorToken{}, nil, fmt.Errorf("malformed cursor token")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var t cursorToken
	if err := dec.Decode(&t); err != nil {
		return cursorToken{}, nil, fmt.Errorf("malformed cursor token")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return cursorToken{}, nil, fmt.Errorf("malformed cursor token")
	}
	if t.ID == "" {
		return cursorToken{}, nil, fmt.Errorf("cursor id must not be empty")
	}
	if t.V == nil {
		return cursorToken{}, nil, fmt.Errorf("cursor value must not be null")
	}

	if t.Version == 0 {
		if t.Type != "" {
			return cursorToken{}, nil, fmt.Errorf("malformed cursor token")
		}
		value, err := decodeLegacyCursorValue(t.V)
		if err != nil {
			return cursorToken{}, nil, err
		}
		return t, value, nil
	}
	if t.Version != cursorTokenVersion {
		return cursorToken{}, nil, fmt.Errorf("unsupported cursor token version")
	}

	switch t.Type {
	case cursorTypeString:
		if _, ok := t.V.(string); !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed string cursor value")
		}
		return t, t.V, nil
	case cursorTypeBoolean:
		if _, ok := t.V.(bool); !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed boolean cursor value")
		}
		return t, t.V, nil
	case cursorTypeInteger:
		n, ok := t.V.(json.Number)
		if !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed integer cursor value")
		}
		i, err := parseCursorInteger(n)
		if err != nil {
			return cursorToken{}, nil, fmt.Errorf("malformed integer cursor value")
		}
		return t, i, nil
	case cursorTypeNumber:
		n, ok := t.V.(json.Number)
		if !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed numeric cursor value")
		}
		f, err := n.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return cursorToken{}, nil, fmt.Errorf("malformed numeric cursor value")
		}
		return t, f, nil
	case cursorTypeTime:
		s, ok := t.V.(string)
		if !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed time cursor value")
		}
		canonical, ok := canonicalTimeValue(s)
		if !ok {
			return cursorToken{}, nil, fmt.Errorf("malformed time cursor value")
		}
		return t, canonical, nil
	case cursorTypeJSON:
		return cursorToken{}, nil, fmt.Errorf("unsupported cursor value type")
	default:
		return cursorToken{}, nil, fmt.Errorf("unknown cursor value type")
	}
}

func parseCursorInteger(n json.Number) (any, error) {
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	if u, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
		return u, nil
	}
	return nil, fmt.Errorf("not an integer")
}

func decodeLegacyCursorValue(v any) (any, error) {
	switch v := v.(type) {
	case string, bool:
		return v, nil
	case json.Number:
		if i, err := parseCursorInteger(v); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("malformed numeric cursor value")
		}
		return f, nil
	default:
		return nil, fmt.Errorf("cursor value must be a string, number, or boolean")
	}
}

// decodeCursorForField validates a token value against the configured cursor
// field and returns the database binding shape for that field. Legacy time
// tokens are canonicalised here; new tokens have already tagged the value as a
// time and stored it canonically.
func decodeCursorForField(token string, field *FieldMeta) (value any, id string, err error) {
	t, value, err := decodeCursorToken(token)
	if err != nil {
		return nil, "", err
	}
	if field == nil {
		return nil, "", fmt.Errorf("cursor field metadata is missing")
	}

	fieldType := field.Type
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	if fieldType == reflect.TypeOf(time.Time{}) {
		if t.Version != 0 && t.Type != cursorTypeTime {
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
		s, ok := value.(string)
		if !ok {
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
		canonical, ok := canonicalTimeValue(s)
		if !ok {
			return nil, "", fmt.Errorf("cursor value is not a timestamp for field %q", field.Tags.DBName)
		}
		return canonical, t.ID, nil
	}

	expected := ""
	switch fieldType.Kind() {
	case reflect.String:
		expected = cursorTypeString
	case reflect.Bool:
		expected = cursorTypeBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		expected = cursorTypeInteger
	case reflect.Float32, reflect.Float64:
		expected = cursorTypeNumber
	}
	if t.Version != 0 && expected != "" && t.Type != expected {
		return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
	}

	switch fieldType.Kind() {
	case reflect.String:
		if _, ok := value.(string); !ok {
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var i int64
		switch n := value.(type) {
		case int64:
			i = n
		case uint64:
			if n > math.MaxInt64 {
				return nil, "", fmt.Errorf("cursor value is out of range for field %q", field.Tags.DBName)
			}
			i = int64(n)
		default:
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
		if reflect.New(fieldType).Elem().OverflowInt(i) {
			return nil, "", fmt.Errorf("cursor value is out of range for field %q", field.Tags.DBName)
		}
		value = i
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var u uint64
		switch n := value.(type) {
		case int64:
			if n < 0 {
				return nil, "", fmt.Errorf("cursor value is out of range for field %q", field.Tags.DBName)
			}
			u = uint64(n)
		case uint64:
			u = n
		default:
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
		// database/sql only accepts unsigned integers through MaxInt64; values
		// above it fail driver conversion even when the Go field is uint64.
		if u > math.MaxInt64 || reflect.New(fieldType).Elem().OverflowUint(u) {
			return nil, "", fmt.Errorf("cursor value is out of range for field %q", field.Tags.DBName)
		}
		value = u
	case reflect.Float32, reflect.Float64:
		switch n := value.(type) {
		case int64:
			value = float64(n)
		case uint64:
			value = float64(n)
		case float64:
		default:
			return nil, "", fmt.Errorf("cursor value type does not match field %q", field.Tags.DBName)
		}
		if n := value.(float64); math.IsInf(n, 0) || math.IsNaN(n) ||
			reflect.New(fieldType).Elem().OverflowFloat(n) {
			return nil, "", fmt.Errorf("cursor value is out of range for field %q", field.Tags.DBName)
		}
	default:
		return nil, "", fmt.Errorf("cursor field %q has unsupported type %s",
			field.Tags.DBName, fieldType)
	}
	return value, t.ID, nil
}

// parseCursorParam builds the CursorParams for a ?cursor= request. vals are the
// raw query values (the first non-empty one is the token; all-empty means the
// first page). It derives the walk direction from a ?sort= on the cursor field
// and rejects sorts on any other field, since keyset ordering is fixed to
// (cursor field, id).
func parseCursorParam(vals []string, model *ModelMeta, sorts []SortExpr) (*CursorParams, error) {
	if model.CursorField == "" {
		return nil, fmt.Errorf(
			"cursor pagination is not enabled for model %s (set ModelConfig.CursorField or mfx:\"cursor_field:...\")",
			model.Name)
	}
	cur := &CursorParams{Field: model.CursorField, Direction: SortAsc}

	for _, s := range sorts {
		if s.IsNested || s.DBName != model.CursorField {
			return nil, fmt.Errorf(
				"sort on %q is not supported with cursor pagination; only the cursor field %q may set the walk direction",
				cursorSortName(s), model.CursorField)
		}
		cur.Direction = s.Direction
	}

	var token string
	for _, v := range vals {
		if v != "" {
			token = v
			break
		}
	}
	if token != "" {
		val, id, err := decodeCursorForField(token, model.FieldByDBName(model.CursorField))
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		cur.HasToken = true
		cur.AfterValue = val
		cur.AfterID = id
	}
	return cur, nil
}

// cursorSortName returns a human-readable name for a rejected sort in a cursor
// error message.
func cursorSortName(s SortExpr) string {
	if s.IsNested {
		return s.RelationKey + "." + s.NestedField
	}
	return s.DBName
}

// ensureCols appends each wanted column to cols if not already present. Used to
// keep the cursor field and id in a ?select= projection so the next-page token
// can be built.
func ensureCols(cols []string, want ...string) []string {
	for _, w := range want {
		if !slices.Contains(cols, w) {
			cols = append(cols, w)
		}
	}
	return cols
}

// collectCursorField resolves the model's keyset pagination column from
// ModelConfig.CursorField (preferred) or an mfx:"cursor_field:..." field tag,
// storing the resolved DB column on m.CursorField. Leaves it "" when the model
// opts out. The named field must exist and be sortable — keyset pagination
// orders by it, and the sortable requirement lets clients flip the walk
// direction with ?sort=<field>:desc.
func (m *ModelMeta) collectCursorField() error {
	raw := m.Config.CursorField
	if raw == "" {
		// Fall back to a field-level tag. Reject conflicting declarations so an
		// accidental second cursor_field is caught rather than silently ignored.
		for _, f := range m.Fields {
			if f.Tags.CursorField == "" {
				continue
			}
			if raw != "" && raw != f.Tags.CursorField {
				return fmt.Errorf(
					"maniflex: model %q declares conflicting cursor_field tags %q and %q",
					m.Name, raw, f.Tags.CursorField)
			}
			raw = f.Tags.CursorField
		}
	}
	if raw == "" {
		return nil
	}

	f := m.FieldByJSONName(raw)
	if f == nil {
		f = m.FieldByDBName(raw)
	}
	if f == nil {
		return fmt.Errorf("maniflex: model %q cursor_field %q is not a field on the model", m.Name, raw)
	}
	if !f.Tags.Sortable {
		return fmt.Errorf(
			"maniflex: model %q cursor_field %q must be mfx:\"sortable\" (keyset pagination orders by it)",
			m.Name, raw)
	}
	// A nullable column has no total order, and the two drivers don't even agree
	// where NULLs belong (Postgres sorts them last on ASC, SQLite first). The
	// keyset boundary predicate compares with > / <, which is never true for NULL,
	// so rows with a NULL cursor value would be silently skipped — or duplicated
	// across pages, depending on the driver. Reject the model instead of paginating
	// it wrongly (BUG-8). Pointer fields are exactly the ones the migrator declares
	// NULL; everything else is NOT NULL.
	if f.Type.Kind() == reflect.Pointer {
		return fmt.Errorf(
			"maniflex: model %q cursor_field %q is nullable (%s); keyset pagination requires a NOT NULL "+
				"column because a NULL has no place in a total order — rows would be skipped or repeated "+
				"across pages. Use a non-pointer field, or pick another cursor field",
			m.Name, raw, f.Type)
	}
	if !isSupportedCursorFieldType(f.Type) {
		return fmt.Errorf(
			"maniflex: model %q cursor_field %q has unsupported type %s; "+
				"use a string, boolean, integer, float, or time.Time field",
			m.Name, raw, f.Type)
	}
	m.CursorField = f.Tags.DBName
	return nil
}

func isSupportedCursorFieldType(t reflect.Type) bool {
	if t == reflect.TypeOf(time.Time{}) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// collectSearchFields resolves every mfx:"searchable" field into m.SearchFields
// (DB column names, declaration order), enabling full-text search (?q=) on the
// model. Full-text search indexes text, so a non-string searchable field is a
// configuration error rejected here rather than producing a broken FTS index at
// AutoMigrate. It also validates ModelConfig.SearchLanguage, which is embedded
// into the Postgres tsvector SQL as a config identifier.
func (m *ModelMeta) collectSearchFields() error {
	var out []string
	for _, f := range m.Fields {
		if !f.Tags.Searchable {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.String {
			return fmt.Errorf(
				"maniflex: model %q field %q is mfx:\"searchable\" but its type is %s; "+
					"full-text search only indexes text (string) columns",
				m.Name, f.Name, f.Type)
		}
		out = append(out, f.Tags.DBName)
	}
	if lang := m.Config.SearchLanguage; lang != "" && !isSearchLangIdent(lang) {
		return fmt.Errorf(
			"maniflex: model %q SearchLanguage %q must be a plain identifier ([A-Za-z_]+)",
			m.Name, lang)
	}
	m.SearchFields = out
	return nil
}

// isSearchLangIdent reports whether s is a plain identifier safe to embed into
// SQL as a text-search configuration name (no quoting/bind possible there).
func isSearchLangIdent(s string) bool {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return s != ""
}
