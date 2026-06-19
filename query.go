package maniflex

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var filterKeyRe = regexp.MustCompile(`^filter(?:\[(\d+)\])?$`)

const (
	defaultPage  = 1
	defaultLimit = 20
	maxLimit     = 200
)

// SortDir is a sort direction.
type SortDir string

const (
	SortAsc  SortDir = "asc"
	SortDesc SortDir = "desc"
)

// SortExpr is a parsed sort instruction.
type SortExpr struct {
	DBName    string  // DB column name
	Direction SortDir // asc or desc

	// IsLocale is true when the sort column is a LocaleString field and the
	// sort should use a JSON path expression targeting LocaleKey.
	IsLocale  bool
	LocaleKey string // e.g. "ar" — set by locale enrichment in the Deserialize step

	// Nested sort — the sort name contained a "." referencing a BelongsTo
	// relation, e.g. ?sort=vendor.name:asc. Mirrors the nested fields on
	// FilterExpr; the query builder adds a LEFT JOIN and orders on the
	// related table's column.
	IsNested      bool
	RelationKey   string // relation key / JOIN alias, e.g. "vendor"
	RelationModel string // related model struct name, e.g. "Vendor"
	RelationTable string // DB table of related model, e.g. "vendors"
	RelationFK    string // FK column on THIS table, e.g. "vendor_id"
	NestedField   string // DB column on the related table, e.g. "name"
}

// QueryParams holds all URL query parameters parsed for a list request.
type QueryParams struct {
	Page     int
	Limit    int
	Filters  []*FilterExpr
	Sorts    []SortExpr
	Includes []string // relation keys to load inline
	Fields   []string // DB column names to SELECT; empty = SELECT table.*

	// Search holds the trimmed ?q= full-text search query, "" when absent. When
	// non-empty the DB step adds the driver's native FTS predicate over the
	// model's mfx:"searchable" columns and orders results by relevance. Only set
	// on models that declare searchable fields; ParseQueryParams rejects ?q= on
	// any other model with a 400.
	Search string

	// Cursor, when non-nil, switches the list query to keyset (cursor)
	// pagination: the DB step walks the dataset ordered by (cursor field, id)
	// with a WHERE bound instead of LIMIT/OFFSET, and skips the COUNT. Set by
	// ParseQueryParams when ?cursor= is present on a cursor-enabled model.
	Cursor *CursorParams
}

// Offset returns the DB offset for the current page.
func (q *QueryParams) Offset() int {
	if q.Page < 1 {
		return 0
	}
	return (q.Page - 1) * q.Limit
}

// HasInclude reports whether key is in the include list.
func (q *QueryParams) HasInclude(key string) bool {
	for _, k := range q.Includes {
		if k == key {
			return true
		}
	}
	return false
}

// ParseQueryParams parses page, limit, filter, sort, and include parameters.
//
//	?page=2&limit=25
//	?filter=status:eq:active&filter=author.role:neq:banned
//	?sort=created_at:desc,title:asc
//	?include=author,category
func ParseQueryParams(r *http.Request, model *ModelMeta, reg RegistryAccessor) (*QueryParams, error) {
	q := &QueryParams{Page: defaultPage, Limit: defaultLimit}
	query := r.URL.Query()

	// ── pagination ────────────────────────────────────────────────────────────
	if p := query.Get("page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid page %q", p)
		}
		q.Page = n
	}
	if l := query.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid limit %q", l)
		}
		if n > maxLimit {
			n = maxLimit
		}
		q.Limit = n
	}

	// ── filters ───────────────────────────────────────────────────────────────
	// Accepts both ?filter=x and ?filter[N]=x (bracket syntax for OR groups).
	// Keys like ?filter[non-digit]=x are rejected with a 400.
	for key, vals := range query {
		m := filterKeyRe.FindStringSubmatch(key)
		if m == nil {
			// Not a filter key at all — ignore.
			// But reject filter[anything-non-digit] explicitly.
			if strings.HasPrefix(key, "filter[") {
				return nil, fmt.Errorf("invalid filter key %q: bracket index must be a non-negative integer (e.g. filter[0])", key)
			}
			continue
		}
		// No bracket → ungrouped (-1). Bracket ?filter[N]= → OR group, mapped
		// onto N+1 so that the internal sentinel for "ungrouped" (Group <= 0,
		// incl. the FilterExpr zero value) stays distinct from a user's group 0.
		group := -1
		if m[1] != "" {
			n, _ := strconv.Atoi(m[1])
			group = n + 1
		}
		for _, raw := range vals {
			expr, err := ParseFilterParam(raw, model, reg)
			if err != nil {
				return nil, fmt.Errorf("filter: %w", err)
			}
			expr.Group = group
			q.Filters = append(q.Filters, expr)
		}
	}

	// Validate: all filters in the same OR group must target the same table.
	if err := validateFilterGroups(q.Filters, model.TableName); err != nil {
		return nil, err
	}

	// ── sort ──────────────────────────────────────────────────────────────────
	// ?sort=created_at:desc,title:asc
	if s := query.Get("sort"); s != "" {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			sp := strings.SplitN(part, ":", 2)
			name := sp[0]
			dir := SortAsc
			if len(sp) == 2 {
				switch strings.ToLower(sp[1]) {
				case "desc":
					dir = SortDesc
				case "asc":
					dir = SortAsc
				default:
					return nil, fmt.Errorf("invalid sort direction %q (want asc or desc)", sp[1])
				}
			}
			// A "." references a BelongsTo relation field, e.g. vendor.name.
			if strings.Contains(name, ".") {
				se, err := resolveNestedSort(name, dir, model, reg)
				if err != nil {
					return nil, err
				}
				q.Sorts = append(q.Sorts, se)
				continue
			}
			f := model.FieldByJSONName(name)
			if f == nil {
				f = model.FieldByDBName(name)
			}
			if f == nil {
				return nil, fmt.Errorf("sort field %q not found on model %s", name, model.Name)
			}
			if !f.Tags.Sortable {
				return nil, fmt.Errorf("field %q is not sortable", name)
			}
			q.Sorts = append(q.Sorts, SortExpr{DBName: f.Tags.DBName, Direction: dir})
		}
	}

	// ── includes ─────────────────────────────────────────────────────────────
	if inc := query.Get("include"); inc != "" {
		for _, key := range strings.Split(inc, ",") {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if model.RelationByKey(key) == nil {
				return nil, fmt.Errorf("include %q is not a relation on model %s", key, model.Name)
			}
			q.Includes = append(q.Includes, key)
		}
	}

	// ── select (field projection) ────────────────────────────────────────────
	// ?select=id,name,department → only those columns are SELECTed from the DB.
	// Hidden and write-only fields are still stripped in the response step.
	if sel := query.Get("select"); sel != "" {
		for _, name := range strings.Split(sel, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			f := model.FieldByJSONName(name)
			if f == nil {
				f = model.FieldByDBName(name)
			}
			if f == nil {
				return nil, fmt.Errorf("select field %q not found on model %s", name, model.Name)
			}
			q.Fields = append(q.Fields, f.Tags.DBName)
		}
	}

	// ── full-text search (?q=) ────────────────────────────────────────────────
	// FTS uses the DB's native ranking/stemming/tokenisation over the model's
	// mfx:"searchable" fields and is deliberately separate from ?filter=. An empty
	// value is treated as no search; a non-empty value on a model with no
	// searchable fields is rejected so a typo'd model never silently ignores it.
	if raw := strings.TrimSpace(query.Get("q")); raw != "" {
		if len(model.SearchFields) == 0 {
			return nil, fmt.Errorf(
				"full-text search (?q=) is not enabled for model %s (tag at least one field mfx:\"searchable\")",
				model.Name)
		}
		q.Search = raw
	}

	// ── cursor (keyset pagination) ────────────────────────────────────────────
	// The presence of ?cursor (even with an empty value, which means "first
	// page") switches an opted-in model to keyset mode. It supersedes ?page; the
	// cursor field (+ id) drives the ordering, so any ?sort= is restricted to the
	// cursor field, where it only sets the walk direction.
	if raw, ok := query["cursor"]; ok {
		// Relevance ordering (?q=) and keyset ordering are mutually exclusive —
		// the cursor walk is fixed to (cursor field, id), not match rank.
		if q.Search != "" {
			return nil, fmt.Errorf("full-text search (?q=) cannot be combined with ?cursor= pagination")
		}
		cur, err := parseCursorParam(raw, model, q.Sorts)
		if err != nil {
			return nil, err
		}
		q.Cursor = cur
		q.Sorts = nil // the cursor ordering is authoritative
		// A projection must still carry the cursor field and id so the adapter
		// can build the next-page token from the last returned row.
		if len(q.Fields) > 0 {
			q.Fields = ensureCols(q.Fields, model.CursorField, "id")
		}
	}

	return q, nil
}
