package admin

import (
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"maniflex"
)

// staticSegment is the first path segment under which embedded assets are
// served. A registered model whose table name collides with it is shadowed.
const staticSegment = "static"

// admin holds the resolved state for one mounted panel.
type admin struct {
	cfg       Config
	api       *apiClient
	tmpl      *templateSet
	models    []*maniflex.ModelMeta          // registry order, after whitelist
	byTable   map[string]*maniflex.ModelMeta // table name → visible model
	allByName map[string]*maniflex.ModelMeta // every registered model, by struct name
	static    http.Handler              // embedded/override asset file server
}

func newAdmin(server *maniflex.Server, cfg Config) (*admin, error) {
	ts, err := loadTemplates(cfg.Templates)
	if err != nil {
		return nil, err
	}

	allow := make(map[string]bool, len(cfg.Models))
	for _, n := range cfg.Models {
		allow[n] = true
	}

	var models []*maniflex.ModelMeta
	byTable := map[string]*maniflex.ModelMeta{}
	allByName := map[string]*maniflex.ModelMeta{}
	for _, m := range server.Registry().All() {
		allByName[m.Name] = m
		if len(allow) > 0 && !allow[m.Name] {
			continue
		}
		models = append(models, m)
		byTable[m.TableName] = m
	}

	a := &admin{
		cfg:       cfg,
		api:       &apiClient{handler: server.Handler(), apiPrefix: server.PathPrefix()},
		tmpl:      ts,
		models:    models,
		byTable:   byTable,
		allByName: allByName,
	}
	a.static = a.staticHandler()
	return a, nil
}

// staticHandler builds the file server for the embedded asset bundle, or for
// Config.StaticFS when supplied.
func (a *admin) staticHandler() http.Handler {
	root := fs.FS(embeddedStatic)
	sub := staticSegment
	if a.cfg.StaticFS != nil {
		root, sub = a.cfg.StaticFS, "."
	}
	dir, err := fs.Sub(root, sub)
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix(a.cfg.PathPrefix+"/"+staticSegment+"/", http.FileServer(http.FS(dir)))
}

// routes builds the panel's HTTP routing table. To keep the route set free of
// ServeMux pattern conflicts (a static subtree would otherwise overlap the
// {model}/{id} wildcard), nested paths are dispatched by hand from two
// catch-all handlers — one per HTTP method.
func (a *admin) routes() http.Handler {
	mux := http.NewServeMux()
	p := a.cfg.PathPrefix

	mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p+"/", http.StatusFound)
	})
	mux.HandleFunc("GET "+p+"/{$}", a.handleDashboard)
	mux.HandleFunc("GET "+p+"/{model}", a.handleList)
	mux.HandleFunc("GET "+p+"/{model}/{rest...}", a.getNested)

	if !a.cfg.ReadOnly {
		mux.HandleFunc("POST "+p+"/{model}", a.handleCreate)
		mux.HandleFunc("POST "+p+"/{model}/{rest...}", a.postNested)
	}
	return mux
}

// getNested dispatches GET requests below /{model}: static assets, the create
// form, the edit form, and record detail.
func (a *admin) getNested(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	rest := strings.Trim(r.PathValue("rest"), "/")

	if model == staticSegment {
		a.static.ServeHTTP(w, r)
		return
	}
	meta, ok := a.lookup(w, model)
	if !ok {
		return
	}

	switch {
	case rest == "":
		http.Redirect(w, r, a.cfg.PathPrefix+"/"+model, http.StatusFound)
	case rest == "new":
		if a.cfg.ReadOnly {
			a.renderError(w, http.StatusNotFound, "Panel is read-only.")
			return
		}
		a.handleNew(w, r, meta)
	case strings.HasSuffix(rest, "/edit"):
		if a.cfg.ReadOnly {
			a.renderError(w, http.StatusNotFound, "Panel is read-only.")
			return
		}
		a.handleEdit(w, r, meta, strings.TrimSuffix(rest, "/edit"))
	case strings.ContainsRune(rest, '/'):
		a.renderError(w, http.StatusNotFound, "No such page.")
	default:
		a.handleDetail(w, r, meta, rest)
	}
}

// postNested dispatches POST requests below /{model}: record update and delete.
func (a *admin) postNested(w http.ResponseWriter, r *http.Request) {
	meta, ok := a.lookup(w, r.PathValue("model"))
	if !ok {
		return
	}
	rest := strings.Trim(r.PathValue("rest"), "/")
	switch {
	case strings.HasSuffix(rest, "/delete"):
		a.handleDelete(w, r, meta, strings.TrimSuffix(rest, "/delete"))
	case strings.ContainsRune(rest, '/'):
		a.renderError(w, http.StatusNotFound, "No such page.")
	default:
		a.handleUpdate(w, r, meta, rest)
	}
}

// nav builds the sidebar entries, one per visible model.
func (a *admin) nav() []navItem {
	out := make([]navItem, 0, len(a.models))
	for _, m := range a.models {
		out = append(out, navItem{Label: prettify(m.TableName), Table: m.TableName})
	}
	return out
}

// base returns a viewData pre-filled with the chrome common to every page.
func (a *admin) base(active string) viewData {
	return viewData{
		Title:  a.cfg.Title,
		Prefix: a.cfg.PathPrefix,
		Nav:    a.nav(),
		Active: active,
	}
}

// lookup resolves a table name to a visible model, or writes a 404.
func (a *admin) lookup(w http.ResponseWriter, table string) (*maniflex.ModelMeta, bool) {
	meta, ok := a.byTable[table]
	if !ok {
		a.renderError(w, http.StatusNotFound, "Unknown model: "+table)
		return nil, false
	}
	return meta, true
}

// ─── Dashboard ────────────────────────────────────────────────────────────

// handleDashboard renders the landing page: one card per model with its row
// count, fetched in-process from the API.
func (a *admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cards := make([]dashCard, 0, len(a.models))
	for _, m := range a.models {
		c := dashCard{Label: prettify(m.TableName), Table: m.TableName}
		if page, err := a.api.list(r, m.TableName, "limit=1"); err != nil {
			c.Err = err.Error()
		} else {
			c.Count = page.Total
		}
		cards = append(cards, c)
	}
	vd := a.base("")
	vd.Dashboard = &dashboardData{Cards: cards}
	a.render(w, "dashboard", vd)
}

// ─── List ─────────────────────────────────────────────────────────────────

// handleList renders the paginated table view for one model. Filter and sort
// selections are carried in admin-owned query parameters (f_<field>, sort,
// page) and translated into the API's own query syntax.
func (a *admin) handleList(w http.ResponseWriter, r *http.Request) {
	meta, ok := a.lookup(w, r.PathValue("model"))
	if !ok {
		return
	}

	q := r.URL.Query()
	apiQuery := url.Values{}
	apiQuery.Set("limit", "20")
	if p := q.Get("page"); p != "" {
		apiQuery.Set("page", p)
	}
	if s := q.Get("sort"); s != "" {
		apiQuery.Set("sort", apiSort(s))
	}
	for key, vals := range q {
		field, isFilter := strings.CutPrefix(key, "f_")
		if !isFilter || len(vals) == 0 || vals[0] == "" {
			continue
		}
		apiQuery.Add("filter", apiFilter(meta, field, vals[0]))
	}

	page, err := a.api.list(r, meta.TableName, apiQuery.Encode())
	if err != nil {
		a.renderError(w, http.StatusBadGateway, err.Error())
		return
	}

	cols := displayColumns(meta)
	rows := make([]rowView, 0, len(page.Items))
	for _, item := range page.Items {
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = cellString(item[col.JSONName])
		}
		id, _ := item["id"].(string)
		rows = append(rows, rowView{
			ID:    id,
			Href:  a.cfg.PathPrefix + "/" + meta.TableName + "/" + id,
			Cells: cells,
		})
	}

	ld := &listData{
		Model:    newModelView(meta),
		Columns:  cols,
		Rows:     rows,
		Total:    page.Total,
		Page:     max(page.Page, 1),
		Pages:    page.Pages,
		Limit:    page.Limit,
		Filters:  a.buildFilters(meta, q),
		Sorts:    sortOptions(meta, q.Get("sort")),
		ReadOnly: a.cfg.ReadOnly,
	}
	if !a.cfg.ReadOnly {
		ld.NewHref = a.cfg.PathPrefix + "/" + meta.TableName + "/new"
	}
	if ld.Page > 1 {
		ld.PrevHref = a.pageHref(meta.TableName, q, ld.Page-1)
	}
	if int64(ld.Page) < page.Pages {
		ld.NextHref = a.pageHref(meta.TableName, q, ld.Page+1)
	}

	vd := a.base(meta.TableName)
	vd.List = ld
	a.render(w, "list", vd)
}

// buildFilters produces one filter control per filterable field, pre-filled
// with any value currently applied via the f_<field> query parameter.
func (a *admin) buildFilters(meta *maniflex.ModelMeta, q url.Values) []filterControl {
	var out []filterControl
	for _, f := range meta.Fields {
		if !f.Tags.Filterable {
			continue
		}
		fc := filterControl{
			JSONName: f.Tags.JSONName,
			Label:    prettify(f.Tags.JSONName),
			Value:    q.Get("f_" + f.Tags.JSONName),
		}
		for _, e := range f.Tags.Enum {
			fc.Options = append(fc.Options, optionView{Value: e, Label: e, Selected: e == fc.Value})
		}
		out = append(out, fc)
	}
	return out
}

// pageHref builds a list-view URL for a page number, preserving the active
// filter and sort selections.
func (a *admin) pageHref(table string, q url.Values, page int) string {
	c := cloneValues(q)
	c.Set("page", strconv.Itoa(page))
	return a.cfg.PathPrefix + "/" + table + "?" + c.Encode()
}

// ─── Detail ───────────────────────────────────────────────────────────────

// handleDetail renders one record: its scalar fields plus links to related
// records and collections.
func (a *admin) handleDetail(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string) {
	rec, err := a.api.get(r, meta.TableName, id, "")
	if err != nil {
		a.renderError(w, statusOf(err), err.Error())
		return
	}

	fkRel := fkRelations(meta)
	var fields []detailField
	for _, col := range displayColumns(meta) {
		df := detailField{Label: col.Label, Value: cellString(rec[col.JSONName])}
		if rel, isFK := fkRel[col.JSONName]; isFK && df.Value != "—" {
			if rt := a.relatedTable(rel); rt != "" {
				df.Href = a.cfg.PathPrefix + "/" + rt + "/" + df.Value
			}
		}
		fields = append(fields, df)
	}

	var rels []relationLink
	for i := range meta.Relations {
		rel := &meta.Relations[i]
		if rel.Kind != maniflex.HasMany {
			continue
		}
		rt := a.relatedTable(rel)
		if rt == "" {
			continue
		}
		rels = append(rels, relationLink{
			Label: prettify(rel.RelationKey),
			Href:  a.cfg.PathPrefix + "/" + rt + "?f_" + rel.FKColumn + "=" + url.QueryEscape(id),
		})
	}

	dd := &detailData{
		Model:     newModelView(meta),
		ID:        id,
		Fields:    fields,
		Relations: rels,
		ReadOnly:  a.cfg.ReadOnly,
	}
	if !a.cfg.ReadOnly {
		dd.EditHref = a.cfg.PathPrefix + "/" + meta.TableName + "/" + id + "/edit"
		dd.DelHref = a.cfg.PathPrefix + "/" + meta.TableName + "/" + id + "/delete"
		dd.CSRF = ensureCSRF(w, r)
	}

	vd := a.base(meta.TableName)
	vd.Detail = dd
	a.render(w, "detail", vd)
}

// relatedTable returns the table name of a relation's target model, or "" if
// the target is not a registered model.
func (a *admin) relatedTable(rel *maniflex.RelationMeta) string {
	if m, ok := a.allByName[rel.RelatedModel]; ok {
		return m.TableName
	}
	return ""
}

// ─── Create / Edit forms ──────────────────────────────────────────────────

// handleNew renders the empty create form.
func (a *admin) handleNew(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta) {
	fields := a.buildFormFields(r, meta, nil, false, nil)
	a.renderForm(w, r, meta, "", fields, "")
}

// handleEdit renders the edit form pre-filled from the existing record.
func (a *admin) handleEdit(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string) {
	rec, err := a.api.get(r, meta.TableName, id, "")
	if err != nil {
		a.renderError(w, statusOf(err), err.Error())
		return
	}
	fields := a.buildFormFields(r, meta, rec, true, nil)
	a.renderForm(w, r, meta, id, fields, "")
}

// renderForm writes the shared create/edit form template.
func (a *admin) renderForm(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string, fields []formField, formErr string) {
	action := a.cfg.PathPrefix + "/" + meta.TableName
	if id != "" {
		action += "/" + id
	}
	vd := a.base(meta.TableName)
	vd.Form = &formData{
		Model:     newModelView(meta),
		ID:        id,
		IsEdit:    id != "",
		HasFiles:  meta.HasFileFields(),
		Action:    action,
		CSRF:      ensureCSRF(w, r),
		Fields:    fields,
		FormError: formErr,
	}
	a.render(w, "form", vd)
}

// handleCreate processes a create-form submission.
func (a *admin) handleCreate(w http.ResponseWriter, r *http.Request) {
	meta, ok := a.lookup(w, r.PathValue("model"))
	if !ok {
		return
	}
	if !a.acceptForm(w, r) {
		return
	}
	a.submit(w, r, meta, "", false)
}

// handleUpdate processes an edit-form submission.
func (a *admin) handleUpdate(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string) {
	if !a.acceptForm(w, r) {
		return
	}
	a.submit(w, r, meta, id, true)
}

// handleDelete processes a delete submission and redirects to the list.
func (a *admin) handleDelete(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string) {
	if !a.acceptForm(w, r) {
		return
	}
	if err := a.api.delete(r, meta.TableName, id); err != nil {
		a.renderError(w, statusOf(err), err.Error())
		return
	}
	http.Redirect(w, r, a.cfg.PathPrefix+"/"+meta.TableName, http.StatusSeeOther)
}

// acceptForm parses an incoming form body and verifies its CSRF token. It
// writes the appropriate error page and returns false on any failure.
func (a *admin) acceptForm(w http.ResponseWriter, r *http.Request) bool {
	if err := parseForm(r); err != nil {
		a.renderError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if !checkCSRF(r) {
		a.renderError(w, http.StatusForbidden,
			"CSRF token missing or invalid — reload the form and retry.")
		return false
	}
	return true
}

// submit collects form values, issues the API write, and either redirects to
// the saved record or re-renders the form with inline validation errors.
func (a *admin) submit(w http.ResponseWriter, r *http.Request, meta *maniflex.ModelMeta, id string, isEdit bool) {
	var (
		rec map[string]any
		err error
	)
	if meta.HasFileFields() {
		body, files := collectMultipart(r, meta, isEdit)
		target := a.api.apiPrefix + "/" + meta.TableName
		method := http.MethodPost
		if isEdit {
			target += "/" + id
			method = http.MethodPatch
		}
		rec, err = a.api.writeMultipart(r, method, target, body, files)
	} else {
		body := collectBody(r, meta, isEdit)
		if isEdit {
			rec, err = a.api.update(r, meta.TableName, id, body)
		} else {
			rec, err = a.api.create(r, meta.TableName, body)
		}
	}

	if err != nil {
		if ae, isAPI := err.(*apiError); isAPI {
			fields := a.buildFormFields(r, meta, formSnapshot(r, meta), isEdit, ae.Fields)
			a.renderForm(w, r, meta, id, fields, formErrorText(ae))
			return
		}
		a.renderError(w, http.StatusBadGateway, err.Error())
		return
	}

	newID := id
	if v, ok := rec["id"].(string); ok && v != "" {
		newID = v
	}
	http.Redirect(w, r, a.cfg.PathPrefix+"/"+meta.TableName+"/"+newID, http.StatusSeeOther)
}

// ─── Form helpers ─────────────────────────────────────────────────────────

// buildFormFields projects a model into form inputs. record pre-fills values
// for an edit form; errs supplies inline validation messages after a failed
// submission. Relation options are fetched in-process for each FK field.
func (a *admin) buildFormFields(r *http.Request, meta *maniflex.ModelMeta, record map[string]any, isEdit bool, errs []fieldError) []formField {
	fkRel := fkRelations(meta)
	errByField := map[string]string{}
	for _, e := range errs {
		errByField[e.Field] = e.Message
	}

	var fields []formField
	for _, f := range meta.Fields {
		jn := f.Tags.JSONName
		if jn == "id" || f.Tags.Hidden {
			continue
		}
		readonly := f.Tags.Readonly
		// Readonly fields are server-managed; pointless on a create form.
		if readonly && !isEdit {
			continue
		}
		rel := fkRel[jn]
		ff := formField{
			JSONName: jn,
			Label:    prettify(jn),
			Widget:   widgetFor(f, rel),
			Required: f.Tags.Required && !isEdit,
			Disabled: readonly || (f.Tags.Immutable && isEdit),
			Accept:   strings.Join(f.Tags.Accept, ","),
			Error:    errByField[jn],
		}

		var raw any
		if record != nil {
			raw = record[jn]
		}

		switch ff.Widget {
		case "checkbox":
			ff.Checked = raw == true || raw == "true"
		case "select":
			ff.Options = enumOptions(f.Tags.Enum, formValue(raw), ff.Required)
		case "relation":
			ff.Options = a.relationOptions(r, rel, formValue(raw))
		case "file":
			ff.FileURL = fileURL(a.api.apiPrefix, formValue(raw))
		case "datetime":
			ff.Value = datetimeLocal(formValue(raw))
		default:
			ff.Value = formValue(raw)
		}
		fields = append(fields, ff)
	}
	return fields
}

// relationOptions fetches up to 200 rows of a FK target model and renders them
// as <option> values, marking the one matching current as selected.
func (a *admin) relationOptions(r *http.Request, rel *maniflex.RelationMeta, current string) []optionView {
	opts := []optionView{{Value: "", Label: "— none —", Selected: current == ""}}
	target, ok := a.allByName[rel.RelatedModel]
	if !ok {
		return opts
	}
	page, err := a.api.list(r, target.TableName, "limit=200")
	if err != nil {
		return opts
	}
	title := titleField(target)
	for _, item := range page.Items {
		id, _ := item["id"].(string)
		opts = append(opts, optionView{
			Value:    id,
			Label:    recordLabel(item, title),
			Selected: id == current,
		})
	}
	return opts
}

// fkRelations maps a model's BelongsTo FK JSON field names to their relations.
func fkRelations(m *maniflex.ModelMeta) map[string]*maniflex.RelationMeta {
	out := map[string]*maniflex.RelationMeta{}
	for i := range m.Relations {
		rel := &m.Relations[i]
		if rel.Kind != maniflex.BelongsTo {
			continue
		}
		for j := range m.Fields {
			if m.Fields[j].Name == rel.FieldName {
				out[m.Fields[j].Tags.JSONName] = rel
			}
		}
	}
	return out
}

// writableField reports whether a field accepts a value from a panel form for
// the given operation.
func writableField(f maniflex.FieldMeta, isEdit bool) bool {
	if f.Tags.JSONName == "id" || f.Tags.Hidden || f.Tags.Readonly {
		return false
	}
	return !(f.Tags.Immutable && isEdit)
}

// collectBody assembles a JSON-keyed field map from form values, coercing each
// value to the type implied by its widget. Empty inputs are omitted, so an
// update is naturally a partial PATCH and a create relies on field defaults.
func collectBody(r *http.Request, meta *maniflex.ModelMeta, isEdit bool) map[string]any {
	body := map[string]any{}
	fkRel := fkRelations(meta)
	for _, f := range meta.Fields {
		if !writableField(f, isEdit) {
			continue
		}
		jn := f.Tags.JSONName
		widget := widgetFor(f, fkRel[jn])
		if widget == "checkbox" {
			body[jn] = r.Form[jn] != nil
			continue
		}
		raw := strings.TrimSpace(r.FormValue(jn))
		if raw == "" {
			continue
		}
		if widget == "number" {
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				body[jn] = n
				continue
			}
		}
		body[jn] = raw
	}
	return body
}

// collectMultipart assembles string form values and buffered file parts for a
// model that has mfx:"file" fields.
func collectMultipart(r *http.Request, meta *maniflex.ModelMeta, isEdit bool) (map[string]string, map[string]*uploadedFile) {
	body := map[string]string{}
	files := map[string]*uploadedFile{}
	fkRel := fkRelations(meta)
	for _, f := range meta.Fields {
		if !writableField(f, isEdit) {
			continue
		}
		jn := f.Tags.JSONName
		if f.Tags.File {
			if uf := readUpload(r, jn); uf != nil {
				files[jn] = uf
			}
			continue
		}
		if widgetFor(f, fkRel[jn]) == "checkbox" {
			if r.Form[jn] != nil {
				body[jn] = "true"
			} else {
				body[jn] = "false"
			}
			continue
		}
		if v := strings.TrimSpace(r.FormValue(jn)); v != "" {
			body[jn] = v
		}
	}
	return body, files
}

// readUpload extracts a single buffered file part from a parsed multipart form.
func readUpload(r *http.Request, field string) *uploadedFile {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil
	}
	headers := r.MultipartForm.File[field]
	if len(headers) == 0 {
		return nil
	}
	fh := headers[0]
	f, err := fh.Open()
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &uploadedFile{Filename: fh.Filename, Data: data}
}

// formSnapshot rebuilds a record map from submitted form values so a failed
// submission can be re-rendered with the user's input intact.
func formSnapshot(r *http.Request, meta *maniflex.ModelMeta) map[string]any {
	out := map[string]any{}
	fkRel := fkRelations(meta)
	for _, f := range meta.Fields {
		jn := f.Tags.JSONName
		if widgetFor(f, fkRel[jn]) == "checkbox" {
			out[jn] = r.Form[jn] != nil
			continue
		}
		if v := r.FormValue(jn); v != "" {
			out[jn] = v
		}
	}
	return out
}
