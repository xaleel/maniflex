package admin

import (
	"reflect"
	"time"

	"maniflex"
)

// navItem is one sidebar entry.
type navItem struct {
	Label string
	Table string
}

// viewData is the single root object passed to every template. Only the
// page-specific pointer for the rendered page is non-nil.
type viewData struct {
	Title  string
	Prefix string
	Nav    []navItem
	Active string // table name of the active model, for nav highlighting

	Dashboard *dashboardData
	List      *listData
	Detail    *detailData
	Form      *formData
	Error     *errorData
}

type dashboardData struct {
	Cards []dashCard
}

// dashCard is one model summary tile on the dashboard.
type dashCard struct {
	Label string
	Table string
	Count int64
	Err   string // non-empty if the count could not be fetched
}

type errorData struct {
	Status  int
	Message string
}

// columnView describes one table column in the list view.
type columnView struct {
	JSONName string
	Label    string
	Sortable bool
}

// modelView is the rendering-ready projection of a maniflex.ModelMeta.
type modelView struct {
	Name  string
	Table string
	Label string
}

// ─── List view ────────────────────────────────────────────────────────────

// filterControl is one filter input rendered above the list table.
type filterControl struct {
	JSONName string
	Label    string
	Value    string // currently applied value, for re-display
	Options  []optionView
}

// sortOption is one entry in the list-view sort dropdown.
type sortOption struct {
	Value    string // e.g. "name" or "-name"
	Label    string // e.g. "Name (A→Z)"
	Selected bool
}

// listData backs the list.html template.
type listData struct {
	Model    modelView
	Columns  []columnView
	Rows     []rowView
	Total    int64
	Page     int
	Pages    int64
	Limit    int
	PrevHref string
	NextHref string
	Filters  []filterControl
	Sorts    []sortOption
	NewHref  string
	ReadOnly bool
}

// rowView is one rendered record: cells aligned to Columns, plus a detail link.
type rowView struct {
	ID    string
	Href  string
	Cells []string
}

// ─── Detail view ──────────────────────────────────────────────────────────

// detailField is one label/value row in the detail view.
type detailField struct {
	Label string
	Value string
	Href  string // non-empty → render the value as a link
}

// relationLink points at a related model's list, filtered to this record.
type relationLink struct {
	Label string
	Href  string
}

// detailData backs the detail.html template.
type detailData struct {
	Model     modelView
	ID        string
	Fields    []detailField
	Relations []relationLink
	EditHref  string
	DelHref   string
	CSRF      string
	ReadOnly  bool
}

// ─── Form view ────────────────────────────────────────────────────────────

// optionView is one <option> in a select or relation widget.
type optionView struct {
	Value    string
	Label    string
	Selected bool
}

// formField is one rendered input in a create/edit form.
type formField struct {
	JSONName string
	Label    string
	Widget   string // text|textarea|number|checkbox|select|relation|file|datetime
	Required bool
	Disabled bool
	Value    string
	Checked  bool
	Options  []optionView
	Accept   string // file accept attribute
	FileURL  string // current stored file, for a preview/download link
	Error    string // inline validation message
}

// formData backs the form.html template (shared by create and edit).
type formData struct {
	Model     modelView
	ID        string // empty → create
	IsEdit    bool
	HasFiles  bool   // model has mfx:"file" fields → multipart enctype
	Action    string // POST target
	CSRF      string
	Fields    []formField
	FormError string // a non-field error (e.g. a failed request)
}

// ─── Builders ─────────────────────────────────────────────────────────────

var timeType = reflect.TypeOf(time.Time{})

func newModelView(m *maniflex.ModelMeta) modelView {
	return modelView{Name: m.Name, Table: m.TableName, Label: prettify(m.TableName)}
}

// displayColumns returns the columns shown in list/detail views, honouring the
// hidden and writeonly tags and surfacing the id column first.
func displayColumns(m *maniflex.ModelMeta) []columnView {
	var cols []columnView
	var idCol *columnView
	for _, f := range m.Fields {
		if f.Tags.Hidden || f.Tags.WriteOnly {
			continue
		}
		col := columnView{
			JSONName: f.Tags.JSONName,
			Label:    prettify(f.Tags.JSONName),
			Sortable: f.Tags.Sortable,
		}
		if f.Tags.JSONName == "id" {
			c := col
			idCol = &c
			continue
		}
		cols = append(cols, col)
	}
	if idCol != nil {
		cols = append([]columnView{*idCol}, cols...)
	}
	return cols
}

// titleField picks the best human-readable field to label a record by.
func titleField(m *maniflex.ModelMeta) string {
	for _, pref := range []string{"name", "title", "label", "email", "slug"} {
		if f := m.FieldByJSONName(pref); f != nil {
			return pref
		}
	}
	for _, f := range m.Fields {
		if f.Tags.Hidden || f.Tags.WriteOnly || f.Tags.JSONName == "id" {
			continue
		}
		if kindOf(f.Type) == reflect.String {
			return f.Tags.JSONName
		}
	}
	return "id"
}

// recordLabel renders a short label for one record, e.g. for FK <select>
// options and relation links.
func recordLabel(item map[string]any, title string) string {
	if v, ok := item[title]; ok {
		if s := cellString(v); s != "—" {
			return s
		}
	}
	if id, ok := item["id"].(string); ok {
		return id
	}
	return "—"
}

// kindOf unwraps a pointer type and returns the element kind.
func kindOf(t reflect.Type) reflect.Kind {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Kind()
}

// isNumericKind reports whether k is an integer or float kind.
func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// widgetFor maps a field's type and tags to a form widget name.
func widgetFor(f maniflex.FieldMeta, rel *maniflex.RelationMeta) string {
	switch {
	case rel != nil:
		return "relation"
	case f.Tags.File:
		return "file"
	case len(f.Tags.Enum) > 0:
		return "select"
	}
	bare := f.Type
	for bare.Kind() == reflect.Ptr {
		bare = bare.Elem()
	}
	switch {
	case bare == timeType:
		return "datetime"
	case bare.Kind() == reflect.Bool:
		return "checkbox"
	case isNumericKind(bare.Kind()):
		return "number"
	}
	return "text"
}
