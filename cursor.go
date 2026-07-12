package maniflex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
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

// cursorToken is the wire shape encoded into the opaque ?cursor= string.
type cursorToken struct {
	V  any    `json:"v"`
	ID string `json:"id"`
}

// EncodeCursor builds the opaque token that points just past (value, id) in the
// keyset ordering. It is base64url(JSON) so it survives in a query string and
// carries no separator-collision risk.
func EncodeCursor(value any, id string) string {
	b, err := json.Marshal(cursorToken{V: value, ID: id})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reverses EncodeCursor. JSON numbers are decoded as int64 when
// integral (else float64) so an integer cursor column compares as a number on
// both Postgres and SQLite rather than arriving as a lossy float.
func DecodeCursor(token string) (value any, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, "", fmt.Errorf("malformed cursor token")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var t cursorToken
	if err := dec.Decode(&t); err != nil {
		return nil, "", fmt.Errorf("malformed cursor token")
	}
	return normalizeCursorValue(t.V), t.ID, nil
}

// normalizeCursorValue converts a json.Number into the narrowest stdlib numeric
// type so database/sql binds it as a number. Non-numeric values pass through.
func normalizeCursorValue(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
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
		val, id, err := DecodeCursor(token)
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
	m.CursorField = f.Tags.DBName
	return nil
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
