package maniflex

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

// GenerateSpec builds a complete OpenAPI 3.1.0 document from all registered
// models and custom actions. It is called by the default Generate pipeline step
// and can be called directly by middleware to get the base spec before customisation.
//
// The optional trailing search argument documents the built-in /search endpoint
// when the app enabled it via Server.EnableGlobalSearch; it is variadic only so
// existing direct callers keep compiling.
func GenerateSpec(reg RegistryAccessor, cfg *Config, actions []ActionConfig, search ...*GlobalSearchConfig) *OpenAPISpec {
	models := reg.All()

	spec := &OpenAPISpec{
		OpenAPI: "3.1.0",
		Info: OpenAPIInfo{
			Title:   "maniflex API",
			Version: "1.0.0",
			Description: fmt.Sprintf(
				"Auto-generated REST API. Base path: %s", cfg.PathPrefix),
		},
		Servers: []OpenAPIServer{
			{URL: cfg.PathPrefix, Description: "Default server"},
		},
		Paths:      make(map[string]PathItem),
		Components: OpenAPIComponents{Schemas: make(map[string]*OASSchema)},
	}

	// One tag per model for Swagger UI grouping
	for _, m := range models {
		spec.Tags = append(spec.Tags, OpenAPITag{
			Name:        m.Name,
			Description: fmt.Sprintf("Operations on %s", m.TableName),
		})
	}

	for _, m := range models {
		buildModelSchemas(spec, m, reg)
		// Headless models mount no REST routes, so emit their schema (still
		// referenced by relations and custom actions) but no auto-generated paths.
		if m.Config.Headless {
			continue
		}
		buildModelPaths(spec, m, cfg)
	}

	// Add standalone file endpoints when storage is configured
	if cfg.FilesConfig.MountEndpoints {
		buildFileEndpointPaths(spec)
	}

	buildActionPaths(spec, actions)

	// Built-in cross-model search endpoint (4.10), when enabled.
	if len(search) > 0 && search[0] != nil {
		buildGlobalSearchPath(spec, search[0], reg)
	}

	// Sort paths for deterministic output
	sortedPaths := make(map[string]PathItem, len(spec.Paths))
	keys := make([]string, 0, len(spec.Paths))
	for k := range spec.Paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sortedPaths[k] = spec.Paths[k]
	}
	spec.Paths = sortedPaths

	return spec
}

// ── Schema generation ─────────────────────────────────────────────────────────

// buildModelSchemas adds three schemas to components.schemas for model m:
//
//   - {Name}         — full response schema (excludes writeOnly, includes readOnly)
//   - {Name}Create   — POST body schema (excludes readOnly, marks required fields)
//   - {Name}Update   — PATCH body schema (excludes readOnly, nothing required)
func buildModelSchemas(spec *OpenAPISpec, m *ModelMeta, reg RegistryAccessor) {
	full := &OASSchema{
		Type:       "object",
		Properties: make(map[string]*OASSchema),
	}
	create := &OASSchema{
		Type:       "object",
		Properties: make(map[string]*OASSchema),
	}
	update := &OASSchema{
		Type:       "object",
		Properties: make(map[string]*OASSchema),
	}

	for _, f := range m.Fields {
		if f.Tags.DBName == "id" {
			// id always appears in responses but never in write bodies
			full.Properties["id"] = &OASSchema{Type: "string", Format: "uuid", ReadOnly: true}
			continue
		}

		schema := goTypeToSchema(f.Type)
		if schema == nil {
			continue
		}

		jn := f.Tags.JSONName

		// ── full (response) schema ────────────────────────────────────────────
		if !f.Tags.Hidden {
			s := copySchema(schema)
			if f.Tags.Readonly || f.Tags.Immutable {
				s.ReadOnly = true
			}
			if f.Tags.WriteOnly {
				s.WriteOnly = true
			}
			if len(f.Tags.Enum) > 0 {
				s.Enum = stringsToAny(f.Tags.Enum)
			}
			if f.Tags.Min != nil {
				s.Minimum = f.Tags.Min
			}
			if f.Tags.Max != nil {
				s.Maximum = f.Tags.Max
			}
			if f.Tags.File {
				s.Description = fileFieldDescription(f)
			}
			full.Properties[jn] = s
		}

		// ── create schema ─────────────────────────────────────────────────────
		if !f.Tags.Readonly && !f.Tags.Hidden {
			s := copySchema(schema)
			if f.Tags.WriteOnly {
				s.WriteOnly = true
			}
			if len(f.Tags.Enum) > 0 {
				s.Enum = stringsToAny(f.Tags.Enum)
			}
			if f.Tags.Min != nil {
				s.Minimum = f.Tags.Min
			}
			if f.Tags.Max != nil {
				s.Maximum = f.Tags.Max
			}
			if f.Tags.File {
				s.Description = fileFieldDescription(f) +
					". Supply via multipart upload or as a pre-uploaded storage key string."
			}
			create.Properties[jn] = s
			if f.Tags.Required {
				create.Required = append(create.Required, jn)
			}
		}

		// ── update schema ─────────────────────────────────────────────────────
		// Immutable fields cannot be changed after creation — exclude from PATCH
		if !f.Tags.Readonly && !f.Tags.Hidden && !f.Tags.Immutable {
			s := copySchema(schema)
			if f.Tags.WriteOnly {
				s.WriteOnly = true
			}
			if len(f.Tags.Enum) > 0 {
				s.Enum = stringsToAny(f.Tags.Enum)
			}
			if f.Tags.Min != nil {
				s.Minimum = f.Tags.Min
			}
			if f.Tags.Max != nil {
				s.Maximum = f.Tags.Max
			}
			if f.Tags.File {
				s.Description = fileFieldDescription(f) +
					". Supply via multipart upload or as a pre-uploaded storage key string."
			}
			update.Properties[jn] = s
			// Nothing is required in a PATCH body
		}
	}

	// Embed relation schemas into the full response schema
	for _, rel := range m.Relations {
		// Skip relations whose target model is not registered. The field
		// scanner creates a BelongsTo for every convention FK (e.g. a
		// `RelatedID` field → relation to model "Related") even when no such
		// model exists and no companion struct field resolves it. Emitting a
		// $ref to an absent component schema produces a dangling reference
		// that breaks OpenAPI validators and client codegen (10.7).
		if _, ok := reg.Get(rel.RelatedModel); !ok {
			continue
		}
		switch rel.Kind {
		case BelongsTo:
			full.Properties[rel.RelationKey] = &OASSchema{
				AnyOf: []*OASSchema{
					ref(rel.RelatedModel),
					{Type: "null"},
				},
				Description: fmt.Sprintf("Populated when ?include=%s is requested", rel.RelationKey),
				ReadOnly:    true,
			}
		case HasMany:
			full.Properties[rel.RelationKey] = &OASSchema{
				Type:        "array",
				Items:       ref(rel.RelatedModel),
				Description: fmt.Sprintf("Populated when ?include=%s is requested", rel.RelationKey),
				ReadOnly:    true,
			}
		}
	}

	sort.Strings(create.Required)

	spec.Components.Schemas[m.Name] = full
	spec.Components.Schemas[m.Name+"Create"] = create
	spec.Components.Schemas[m.Name+"Update"] = update

	// Envelope schemas for list and single-record responses
	spec.Components.Schemas[m.Name+"ListResponse"] = listEnvelopeSchema(m.Name)
	spec.Components.Schemas[m.Name+"Response"] = singleEnvelopeSchema(m.Name)
}

// ── Path generation ───────────────────────────────────────────────────────────

func buildModelPaths(spec *OpenAPISpec, m *ModelMeta, cfg *Config) {
	tag := m.Name
	collectionPath := "/" + m.TableName
	itemPath := "/" + m.TableName + "/{id}"

	idParam := OASParameter{
		Name:     "id",
		In:       "path",
		Required: true,
		Schema:   &OASSchema{Type: "string", Format: "uuid"},
	}

	// For models with file fields, offer both JSON and multipart content types.
	createContent := jsonContent(ref(m.Name + "Create"))
	updateContent := jsonContent(ref(m.Name + "Update"))
	if m.HasFileFields() && cfg.FilesConfig.Storage != nil {
		createContent = withMultipartContent(createContent, spec, m, "Create")
		updateContent = withMultipartContent(updateContent, spec, m, "Update")
	}

	// Singleton models expose only GET + PATCH on the bare path (no id, no
	// POST/DELETE/list), matching the routes mountModel registers for them.
	if m.Config.Singleton {
		buildSingletonPaths(spec, m, collectionPath, tag, updateContent)
		return
	}

	// ── Collection: GET + POST ────────────────────────────────────────────────
	spec.Paths[collectionPath] = PathItem{
		Get: &OASOperation{
			OperationID: "list" + m.Name,
			Summary:     "List " + m.Name + " records",
			Tags:        []string{tag},
			Parameters:  listParameters(m),
			Responses: map[string]OASResponse{
				"200": {
					Description: "Paginated list of " + m.Name + " records",
					Content:     jsonContent(ref(m.Name + "ListResponse")),
				},
				"400": errResponse("Invalid query parameters"),
			},
		},
		Post: &OASOperation{
			OperationID: "create" + m.Name,
			Summary:     "Create a " + m.Name,
			Tags:        []string{tag},
			RequestBody: &OASRequestBody{
				Required:    true,
				Description: m.Name + " fields",
				Content:     createContent,
			},
			Responses: map[string]OASResponse{
				"201": {
					Description: m.Name + " created",
					Content:     jsonContent(ref(m.Name + "Response")),
				},
				"400": errResponse("Malformed request body"),
				"422": errResponse("Validation error"),
			},
		},
	}

	// ── Item: GET + PATCH + DELETE ────────────────────────────────────────────
	spec.Paths[itemPath] = PathItem{
		Get: &OASOperation{
			OperationID: "get" + m.Name,
			Summary:     "Get a " + m.Name + " by ID",
			Tags:        []string{tag},
			Parameters:  append([]OASParameter{idParam}, includeParameter(m)),
			Responses: map[string]OASResponse{
				"200": {
					Description: m.Name + " record",
					Content:     jsonContent(ref(m.Name + "Response")),
				},
				"404": errResponse(m.Name + " not found"),
			},
		},
		Patch: &OASOperation{
			OperationID: "update" + m.Name,
			Summary:     "Partially update a " + m.Name,
			Tags:        []string{tag},
			Parameters:  []OASParameter{idParam},
			RequestBody: &OASRequestBody{
				Required:    true,
				Description: "Fields to update",
				Content:     updateContent,
			},
			Responses: map[string]OASResponse{
				"200": {
					Description: "Updated " + m.Name,
					Content:     jsonContent(ref(m.Name + "Response")),
				},
				"404": errResponse(m.Name + " not found"),
				"422": errResponse("Validation error"),
			},
		},
		Delete: &OASOperation{
			OperationID: "delete" + m.Name,
			Summary:     "Delete a " + m.Name,
			Tags:        []string{tag},
			Parameters:  []OASParameter{idParam},
			Responses: map[string]OASResponse{
				"204": {Description: m.Name + " deleted"},
				"404": errResponse(m.Name + " not found"),
			},
		},
	}

	// Per-model attachment routes (3B.3a): one path per mfx:"file" field,
	// mounted only when storage is configured (matches router.go behaviour).
	if cfg.FilesConfig.Storage != nil {
		for _, ff := range m.FileFields() {
			fieldName := ff.Tags.JSONName
			respContent := map[string]OASMediaType{
				"application/octet-stream": {Schema: &OASSchema{Type: "string", Format: "binary"}},
			}
			if len(ff.Tags.Accept) > 0 {
				// Surface the field's accept-list as a schema hint on the
				// response so clients know what MIME types may come back.
				for _, mt := range ff.Tags.Accept {
					if _, exists := respContent[mt]; !exists {
						respContent[mt] = OASMediaType{
							Schema: &OASSchema{Type: "string", Format: "binary"},
						}
					}
				}
			}
			spec.Paths[itemPath+"/"+fieldName] = PathItem{
				Get: &OASOperation{
					OperationID: "get" + m.Name + capitalize(fieldName) + "File",
					Summary:     "Download the " + fieldName + " file of a " + m.Name,
					Description: "Streams the file referenced by the " + m.Name + "." + fieldName +
						" field. Runs through the same Auth pipeline as GET " + itemPath + ".",
					Tags:       []string{tag},
					Parameters: []OASParameter{idParam},
					Responses: map[string]OASResponse{
						"200": {Description: "File contents", Content: respContent},
						"404": errResponse(m.Name + " or file not found"),
					},
				},
			}
		}
	}
}

// buildSingletonPaths registers the GET + PATCH operations for a
// ModelConfig.Singleton model under its bare collection path.
func buildSingletonPaths(spec *OpenAPISpec, m *ModelMeta, collectionPath, tag string, updateContent map[string]OASMediaType) {
	spec.Paths[collectionPath] = PathItem{
		Get: &OASOperation{
			OperationID: "get" + m.Name,
			Summary:     "Get the " + m.Name + " singleton",
			Tags:        []string{tag},
			Responses: map[string]OASResponse{
				"200": {
					Description: m.Name + " record",
					Content:     jsonContent(ref(m.Name + "Response")),
				},
			},
		},
		Patch: &OASOperation{
			OperationID: "update" + m.Name,
			Summary:     "Update the " + m.Name + " singleton",
			Tags:        []string{tag},
			RequestBody: &OASRequestBody{
				Required:    true,
				Description: "Fields to update",
				Content:     updateContent,
			},
			Responses: map[string]OASResponse{
				"200": {
					Description: "Updated " + m.Name,
					Content:     jsonContent(ref(m.Name + "Response")),
				},
				"422": errResponse("Validation error"),
			},
		},
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

// ── Parameter builders ────────────────────────────────────────────────────────

func listParameters(m *ModelMeta) []OASParameter {
	params := []OASParameter{
		{
			Name: "page", In: "query",
			Description: "Page number (1-based)",
			Schema:      &OASSchema{Type: "integer", Minimum: float64ptr(1)},
		},
		{
			Name: "limit", In: "query",
			Description: "Items per page (max 200)",
			Schema:      &OASSchema{Type: "integer", Minimum: float64ptr(1), Maximum: float64ptr(200)},
		},
	}

	// Cursor (keyset) pagination — only for models that declare a cursor field.
	if m.CursorField != "" {
		params = append(params, OASParameter{
			Name: "cursor", In: "query",
			Description: "Opaque keyset-pagination token. Pass an empty value for the first " +
				"page, then the meta.next_cursor from each response to fetch the next. " +
				"Supersedes ?page; ordered by " + m.CursorField + " then id. " +
				"Add ?sort=" + m.CursorField + ":desc to walk in reverse.",
			Schema: &OASSchema{Type: "string"},
		})
	}

	// Full-text search parameter — only for models with mfx:"searchable" fields.
	if len(m.SearchFields) > 0 {
		searchable := filterableFields(m, func(f FieldMeta) bool { return f.Tags.Searchable })
		params = append(params, OASParameter{
			Name: "q", In: "query",
			Description: "Full-text search query. Matches with the database's native " +
				"ranking/stemming over searchable fields and orders results by relevance " +
				"(distinct from ?filter=). Searchable fields: " + strings.Join(searchable, ", ") +
				". Cannot be combined with ?cursor=.",
			Schema: &OASSchema{Type: "string"},
		})
	}

	// Sort parameter — list sortable fields
	sortable := filterableFields(m, func(f FieldMeta) bool { return f.Tags.Sortable || f.Tags.Filterable })
	if len(sortable) > 0 {
		params = append(params, OASParameter{
			Name: "sort", In: "query",
			Description: "Sort expression. Format: `field:asc` or `field:desc`. " +
				"Multiple sorts comma-separated. " +
				"Sortable fields: " + strings.Join(sortable, ", "),
			Schema: &OASSchema{Type: "string"},
		})
	}

	// Filter parameter — list filterable fields
	filterable := filterableFields(m, func(f FieldMeta) bool { return f.Tags.Filterable })
	nestedFilterable := buildNestedFilterDocs(m)
	if len(filterable) > 0 || len(nestedFilterable) > 0 {
		desc := "Filter expression. Format: `field:op:value`. " +
			"Repeatable (?filter=a:eq:1&filter=b:neq:2). " +
			"Use bracket-indexed keys to OR conditions within a group: " +
			"?filter[0]=status:eq:draft&filter[0]=status:eq:published combines as (draft OR published). " +
			"Different group indices are ANDed together. " +
			"Operators: eq, neq, gt, gte, lt, lte, like, ilike, in, not_in, is_null, not_null. " +
			"Filterable fields: " + strings.Join(filterable, ", ")
		if len(nestedFilterable) > 0 {
			desc += ". Nested: " + strings.Join(nestedFilterable, ", ")
		}
		falseVal := false
		params = append(params, OASParameter{
			Name: "filter", In: "query",
			Description: desc,
			Schema:      &OASSchema{Type: "array", Items: &OASSchema{Type: "string"}},
			Style:       "form",
			Explode:     &falseVal,
		})
	}

	// Include parameter — list includable relations
	if len(m.Relations) > 0 {
		params = append(params, includeParameter(m))
	}

	return params
}

func includeParameter(m *ModelMeta) OASParameter {
	keys := make([]string, 0, len(m.Relations))
	for _, r := range m.Relations {
		keys = append(keys, r.RelationKey)
	}
	return OASParameter{
		Name: "include", In: "query",
		Description: "Comma-separated relation keys to embed. Available: " + strings.Join(keys, ", "),
		Schema:      &OASSchema{Type: "string"},
	}
}

func filterableFields(m *ModelMeta, pred func(FieldMeta) bool) []string {
	var out []string
	for _, f := range m.Fields {
		if pred(f) && !f.Tags.Hidden {
			out = append(out, f.Tags.JSONName)
		}
	}
	sort.Strings(out)
	return out
}

func buildNestedFilterDocs(m *ModelMeta) []string {
	var out []string
	for _, rel := range m.Relations {
		if rel.Kind != BelongsTo {
			continue
		}
		out = append(out, rel.RelationKey+".{field}")
	}
	return out
}

// ── Envelope schemas ──────────────────────────────────────────────────────────

func listEnvelopeSchema(modelName string) *OASSchema {
	return &OASSchema{
		Type: "object",
		Properties: map[string]*OASSchema{
			"data": {
				Type:  "array",
				Items: ref(modelName),
			},
			"meta": {
				Type: "object",
				Properties: map[string]*OASSchema{
					"total": {Type: "integer"},
					"page":  {Type: "integer"},
					"limit": {Type: "integer"},
					"pages": {Type: "integer"},
				},
			},
		},
	}
}

func singleEnvelopeSchema(modelName string) *OASSchema {
	return &OASSchema{
		Type: "object",
		Properties: map[string]*OASSchema{
			"data": ref(modelName),
		},
	}
}

// ── Type mapping ──────────────────────────────────────────────────────────────

// goTypeToSchema converts a reflect.Type to an OASSchema.
// Returns nil for types that have no sensible JSON representation.
func goTypeToSchema(t reflect.Type) *OASSchema {
	isPtr := t.Kind() == reflect.Ptr
	if isPtr {
		t = t.Elem()
	}

	var s *OASSchema

	if t == reflect.TypeOf(time.Time{}) {
		s = &OASSchema{Type: "string", Format: "date-time"}
	} else {
		switch t.Kind() {
		case reflect.String:
			s = &OASSchema{Type: "string"}
		case reflect.Bool:
			s = &OASSchema{Type: "boolean"}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
			s = &OASSchema{Type: "integer", Format: "int32"}
		case reflect.Int64:
			s = &OASSchema{Type: "integer", Format: "int64"}
		case reflect.Float32:
			s = &OASSchema{Type: "number", Format: "float"}
		case reflect.Float64:
			s = &OASSchema{Type: "number", Format: "double"}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			s = &OASSchema{Type: "integer", Minimum: float64ptr(0)}
		default:
			return nil
		}
	}

	if isPtr {
		s = nullable(s)
	}
	return s
}

// ── Small helpers ─────────────────────────────────────────────────────────────

func copySchema(s *OASSchema) *OASSchema {
	if s == nil {
		return nil
	}
	cp := *s
	// Deep-copy type slice if present
	if arr, ok := s.Type.([]string); ok {
		cp2 := make([]string, len(arr))
		copy(cp2, arr)
		cp.Type = cp2
	}
	return &cp
}

func jsonContent(s *OASSchema) map[string]OASMediaType {
	return map[string]OASMediaType{"application/json": {Schema: s}}
}

func errResponse(desc string) OASResponse {
	return OASResponse{
		Description: desc,
		Content: jsonContent(&OASSchema{
			Type: "object",
			Properties: map[string]*OASSchema{
				"error": {
					Type: "object",
					Properties: map[string]*OASSchema{
						"code":    {Type: "string"},
						"message": {Type: "string"},
						"details": {Type: "object"},
					},
				},
			},
		}),
	}
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func float64ptr(f float64) *float64 { return &f }

// ── File upload helpers ──────────────────────────────────────────────────────

// withMultipartContent adds a multipart/form-data media type alongside the
// existing application/json content. File fields are declared as binary with
// per-part encoding entries so tooling knows which parts carry file bytes.
func withMultipartContent(
	content map[string]OASMediaType,
	spec *OpenAPISpec,
	m *ModelMeta,
	suffix string, // "Create" or "Update"
) map[string]OASMediaType {
	schemaName := m.Name + suffix + "Multipart"

	// Copy properties and required list from the base JSON schema.
	props := make(map[string]*OASSchema)
	var required []string
	if base, ok := spec.Components.Schemas[m.Name+suffix]; ok {
		for k, v := range base.Properties {
			props[k] = v
		}
		required = append(required, base.Required...)
	}

	// Override file fields: binary format in schema + encoding entry.
	encoding := make(map[string]OASEncoding)
	for _, f := range m.FileFields() {
		jn := f.Tags.JSONName
		props[jn] = &OASSchema{
			Type:        "string",
			Format:      "binary",
			Description: fileFieldDescription(f),
		}
		enc := OASEncoding{ContentType: "application/octet-stream"}
		if len(f.Tags.Accept) == 1 && !strings.Contains(f.Tags.Accept[0], "*") {
			// Single concrete MIME — use it directly.
			enc.ContentType = f.Tags.Accept[0]
		} else if len(f.Tags.Accept) > 0 {
			enc.ContentType = strings.Join(f.Tags.Accept, ", ")
		}
		encoding[jn] = enc
	}

	spec.Components.Schemas[schemaName] = &OASSchema{
		Type:       "object",
		Properties: props,
		Required:   required,
	}

	content["multipart/form-data"] = OASMediaType{
		Schema:   ref(schemaName),
		Encoding: encoding,
	}
	return content
}

// fileMetaSchema returns the reusable schema for a FileMeta response object,
// registering it in spec.Components.Schemas["FileMeta"] on first call.
func fileMetaSchema(spec *OpenAPISpec) *OASSchema {
	if _, exists := spec.Components.Schemas["FileMeta"]; !exists {
		spec.Components.Schemas["FileMeta"] = &OASSchema{
			Type: "object",
			Properties: map[string]*OASSchema{
				"key":          {Type: "string", Description: "Storage key — pass this to GET/DELETE /files/{key}"},
				"content_type": {Type: "string", Description: "MIME type of the stored file"},
				"size":         {Type: "integer", Format: "int64", Description: "File size in bytes"},
				"filename":     {Type: "string", Description: "Original filename supplied by the client"},
			},
		}
	}
	return ref("FileMeta")
}

// fileFieldDescription builds a concise description for a Server:"file" field,
// including max_size and accept constraints when present.
func fileFieldDescription(f FieldMeta) string {
	desc := "Storage key for an uploaded file"
	if f.Tags.MaxSize > 0 {
		desc += fmt.Sprintf(" (max %s)", formatByteSize(f.Tags.MaxSize))
	}
	if len(f.Tags.Accept) > 0 {
		desc += fmt.Sprintf("; accepted types: %s", strings.Join(f.Tags.Accept, ", "))
	}
	return desc
}

// buildFileEndpointPaths adds OpenAPI documentation for the standalone
// POST /files, GET /files/{key}, and DELETE /files/{key} endpoints.
func buildFileEndpointPaths(spec *OpenAPISpec) {
	spec.Tags = append(spec.Tags, OpenAPITag{
		Name:        "Files",
		Description: "Standalone file upload, download, and deletion",
	})

	keyParam := OASParameter{
		Name:        "key",
		In:          "path",
		Required:    true,
		Description: "Storage key of the file (returned by upload)",
		Schema:      &OASSchema{Type: "string"},
	}

	// POST /files
	spec.Paths["/files"] = PathItem{
		Post: &OASOperation{
			OperationID: "uploadFile",
			Summary:     "Upload a file",
			Tags:        []string{"Files"},
			RequestBody: &OASRequestBody{
				Required: true,
				Content: map[string]OASMediaType{
					"multipart/form-data": {
						Schema: &OASSchema{
							Type: "object",
							Properties: map[string]*OASSchema{
								"file": {Type: "string", Format: "binary", Description: "The file to upload"},
							},
							Required: []string{"file"},
						},
					},
				},
			},
			Responses: map[string]OASResponse{
				"201": {
					Description: "File uploaded successfully",
					Content: jsonContent(&OASSchema{
						Type: "object",
						Properties: map[string]*OASSchema{
							"data": fileMetaSchema(spec),
						},
					}),
				},
				"400": errResponse("Invalid multipart request"),
			},
		},
	}

	// GET + DELETE /files/{key}
	spec.Paths["/files/{key}"] = PathItem{
		Get: &OASOperation{
			OperationID: "getFile",
			Summary:     "Download a file by key",
			Tags:        []string{"Files"},
			Parameters:  []OASParameter{keyParam},
			Responses: map[string]OASResponse{
				"200": {Description: "File content (Content-Type set from stored metadata)"},
				"404": errResponse("File not found"),
			},
		},
		Delete: &OASOperation{
			OperationID: "deleteFile",
			Summary:     "Delete a file by key",
			Tags:        []string{"Files"},
			Parameters:  []OASParameter{keyParam},
			Responses: map[string]OASResponse{
				"204": {Description: "File deleted"},
				"404": errResponse("File not found"),
			},
		},
	}
}

// ── Action path generation ────────────────────────────────────────────────────

// buildActionResponses converts the caller-supplied map[int]*OASSchema into the
// map[string]OASResponse that OASOperation expects.
//
// When responses is nil the default set is returned so existing actions that
// omit the field get sensible fallback docs without any change.
//
// A nil schema for a given status means no response body (e.g. 204 No Content).
func buildActionResponses(responses map[int]*OASSchema) map[string]OASResponse {
	if responses == nil {
		return map[string]OASResponse{
			"200": {Description: "OK"},
			"400": errResponse("Bad request"),
			"401": errResponse("Unauthorized"),
			"403": errResponse("Forbidden"),
			"500": errResponse("Internal server error"),
		}
	}
	out := make(map[string]OASResponse, len(responses))
	for code, schema := range responses {
		desc := http.StatusText(code)
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", code)
		}
		r := OASResponse{Description: desc}
		if schema != nil {
			r.Content = jsonContent(schema)
		}
		out[fmt.Sprintf("%d", code)] = r
	}
	return out
}

func buildActionPaths(spec *OpenAPISpec, actions []ActionConfig) {
	// Declare the default "Actions" tag at the document level when any action
	// relies on it, so Swagger UI can render a described group and strict OAS
	// validators don't flag an undeclared tag.
	if anyActionUsesDefaultTag(actions) && !specHasTag(spec, "Actions") {
		spec.Tags = append(spec.Tags, OpenAPITag{
			Name:        "Actions",
			Description: "Custom action endpoints",
		})
	}

	for _, a := range actions {
		tags := a.Tags
		if len(tags) == 0 {
			tags = []string{"Actions"}
		}

		// Path params are always auto-extracted; query params from the
		// optional OpenAPI block are appended after them.
		params := extractPathParams(a.Path)
		params = append(params, a.OpenAPI.QueryParams...)

		// Request body: the inline RequestBody wins; otherwise reflect the
		// OpenAPI.RequestSchema struct into an application/json body.
		reqBody := a.RequestBody
		if reqBody == nil {
			if s := reflectSchema(a.OpenAPI.RequestSchema); s != nil {
				reqBody = JSONRequestBody(s)
			}
		}

		// Responses: the inline Responses map wins; otherwise start from the
		// default set and fold in the reflected OpenAPI.ResponseSchema as the
		// success response.
		responses := buildActionResponses(a.Responses)
		if a.Responses == nil {
			if s := reflectSchema(a.OpenAPI.ResponseSchema); s != nil {
				responses = withSuccessResponse(responses, a.OpenAPI.ResponseStatus, s)
			}
		}

		op := &OASOperation{
			OperationID: actionOperationID(a.Method, a.Path),
			Summary:     a.Summary,
			Description: a.OpenAPI.Description,
			Deprecated:  a.Deprecated,
			Tags:        tags,
			Parameters:  params,
			RequestBody: reqBody,
			Responses:   responses,
			Security:    a.OpenAPI.Security,
		}

		// Model paths in buildModelPaths are stored relative to the server URL
		// (which already carries cfg.PathPrefix); actions must match so every
		// path resolves against the same base. Prefixing here would double it
		// (e.g. /api/api/...).
		specPath := a.Path
		item := spec.Paths[specPath]
		switch strings.ToUpper(a.Method) {
		case "GET":
			item.Get = op
		case "POST":
			item.Post = op
		case "PATCH":
			item.Patch = op
		case "PUT":
			item.Put = op
		case "DELETE":
			item.Delete = op
		}
		spec.Paths[specPath] = item
	}
}

// buildGlobalSearchPath documents the built-in cross-model search endpoint
// (Server.EnableGlobalSearch) under cfg.Path: a GET taking q/limit/models query
// params and returning the merged {"data": [{model, id, snippet, score}]} list.
func buildGlobalSearchPath(spec *OpenAPISpec, cfg *GlobalSearchConfig, reg RegistryAccessor) {
	if !specHasTag(spec, "Search") {
		spec.Tags = append(spec.Tags, OpenAPITag{
			Name:        "Search",
			Description: "Cross-model full-text search",
		})
	}

	var names []string
	for _, m := range reg.All() {
		if m.Config.GlobalSearchable && len(m.SearchFields) > 0 {
			names = append(names, m.Name)
		}
	}
	allowed := strings.Join(names, ", ")

	resultSchema := &OASSchema{
		Type: "object",
		Properties: map[string]*OASSchema{
			"model":   {Type: "string"},
			"id":      {Type: "string"},
			"snippet": {Type: "string"},
			"score":   {Type: "number"},
		},
	}
	bodySchema := &OASSchema{
		Type: "object",
		Properties: map[string]*OASSchema{
			"data": {Type: "array", Items: resultSchema},
		},
	}

	op := &OASOperation{
		OperationID: "globalSearch",
		Summary:     "Cross-model full-text search",
		Description: "Searches every GlobalSearchable model and returns merged, " +
			"relevance-ranked hits. Searchable models: " + allowed + ".",
		Tags: []string{"Search"},
		Parameters: []OASParameter{
			{
				Name: "q", In: "query", Required: true,
				Description: "Full-text search query (required).",
				Schema:      &OASSchema{Type: "string"},
			},
			{
				Name: "limit", In: "query",
				Description: fmt.Sprintf("Maximum merged results (default %d, max %d).",
					cfg.DefaultLimit, cfg.MaxLimit),
				Schema: &OASSchema{Type: "integer", Minimum: float64ptr(0)},
			},
			{
				Name: "models", In: "query",
				Description: "Comma-separated subset of models to search (default: all). " +
					"Allowed: " + allowed + ".",
				Schema: &OASSchema{Type: "string"},
			},
		},
		Responses: map[string]OASResponse{
			"200": {
				Description: "Merged, relevance-ranked search hits.",
				Content: map[string]OASMediaType{
					"application/json": {Schema: bodySchema},
				},
			},
			"400": {Description: "Missing ?q=, invalid ?limit=, or unknown/unexposed ?models= entry."},
		},
	}

	item := spec.Paths[cfg.Path]
	item.Get = op
	spec.Paths[cfg.Path] = item
}

// anyActionUsesDefaultTag reports whether at least one action has no explicit
// Tags and will therefore fall back to the synthetic "Actions" tag.
func anyActionUsesDefaultTag(actions []ActionConfig) bool {
	for _, a := range actions {
		if len(a.Tags) == 0 {
			return true
		}
	}
	return false
}

// specHasTag reports whether spec already declares a top-level tag named name.
func specHasTag(spec *OpenAPISpec, name string) bool {
	for _, t := range spec.Tags {
		if t.Name == name {
			return true
		}
	}
	return false
}

// extractPathParams returns OASParameter entries for every {param} segment
// in a chi-style path. E.g. "/appointments/{id}/cancel" → [{id, path, required}].
func extractPathParams(path string) []OASParameter {
	var params []OASParameter
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			params = append(params, OASParameter{
				Name:     seg[1 : len(seg)-1],
				In:       "path",
				Required: true,
				Schema:   &OASSchema{Type: "string"},
			})
		}
	}
	return params
}

// actionOperationID converts a method+path into a camelCase operationId.
// "POST /appointments/{id}/cancel" → "postAppointmentsIdCancel"
func actionOperationID(method, path string) string {
	parts := strings.FieldsFunc(
		strings.ToLower(method+"_"+path),
		func(r rune) bool { return r == '/' || r == '{' || r == '}' || r == '-' || r == '_' },
	)
	if len(parts) == 0 {
		return strings.ToLower(method)
	}
	result := parts[0]
	for _, p := range parts[1:] {
		if len(p) > 0 {
			result += strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return result
}

// formatByteSize converts bytes to a human-readable string for documentation.
func formatByteSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%dGB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dMB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%dKB", b/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// ── Struct → schema reflection (action OpenAPI, 10.8) ─────────────────────────

// withSuccessResponse folds a reflected response schema into an action's
// response set as the success response. status defaults to 200 when zero; a
// non-200 success status replaces the default "200" entry so the spec does not
// carry a redundant empty 200 response alongside the documented one.
func withSuccessResponse(responses map[string]OASResponse, status int, schema *OASSchema) map[string]OASResponse {
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		delete(responses, fmt.Sprintf("%d", http.StatusOK))
	}
	desc := http.StatusText(status)
	if desc == "" {
		desc = fmt.Sprintf("HTTP %d", status)
	}
	responses[fmt.Sprintf("%d", status)] = OASResponse{
		Description: desc,
		Content:     jsonContent(schema),
	}
	return responses
}

// reflectSchema converts a Go value, pointer, or reflect.Type into an OASSchema
// using the same json + mfx tag conventions as model fields. It returns nil for
// a nil input or a type with no JSON representation. This lets actions document
// their request/response bodies from plain Go structs instead of hand-written
// *OASSchema values.
func reflectSchema(v any) *OASSchema {
	if v == nil {
		return nil
	}
	rt, ok := v.(reflect.Type)
	if !ok {
		rt = reflect.TypeOf(v)
	}
	return reflectTypeSchema(rt, 0)
}

// maxReflectSchemaDepth bounds recursion so self-referential structs cannot
// produce an infinitely nested schema.
const maxReflectSchemaDepth = 10

func reflectTypeSchema(rt reflect.Type, depth int) *OASSchema {
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt == nil {
		return nil
	}
	if rt == reflect.TypeOf(time.Time{}) {
		return goTypeToSchema(rt)
	}
	switch rt.Kind() {
	case reflect.Struct:
		if depth >= maxReflectSchemaDepth {
			return &OASSchema{Type: "object"}
		}
		return reflectStructSchema(rt, depth)
	case reflect.Slice, reflect.Array:
		items := reflectTypeSchema(rt.Elem(), depth+1)
		if items == nil {
			return nil
		}
		return &OASSchema{Type: "array", Items: items}
	case reflect.Map:
		allow := true
		return &OASSchema{Type: "object", AdditionalProperties: &allow}
	default:
		return goTypeToSchema(rt)
	}
}

func reflectStructSchema(rt reflect.Type, depth int) *OASSchema {
	out := &OASSchema{Type: "object", Properties: make(map[string]*OASSchema)}
	var required []string

	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		tags := parseFieldTags(sf)
		if tags.Ignore || tags.Hidden {
			continue
		}

		// Anonymous embedded struct with no json name: promote its fields,
		// matching Go's own JSON-encoding behaviour for embedded structs.
		if sf.Anonymous && sf.Tag.Get("json") == "" {
			et := sf.Type
			for et.Kind() == reflect.Ptr {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && et != reflect.TypeOf(time.Time{}) {
				if sub := reflectTypeSchema(et, depth+1); sub != nil {
					for k, v := range sub.Properties {
						out.Properties[k] = v
					}
					required = append(required, sub.Required...)
				}
				continue
			}
		}

		schema := reflectTypeSchema(sf.Type, depth+1)
		if schema == nil {
			continue
		}
		applyConstraintTags(schema, tags)
		out.Properties[tags.JSONName] = schema
		if tags.Required {
			required = append(required, tags.JSONName)
		}
	}

	sort.Strings(required)
	out.Required = required
	return out
}

// applyConstraintTags copies the validation/visibility directives shared by all
// mfx-tagged fields onto a freshly built schema.
func applyConstraintTags(s *OASSchema, t FieldTags) {
	if len(t.Enum) > 0 {
		s.Enum = stringsToAny(t.Enum)
	}
	if t.Min != nil {
		s.Minimum = t.Min
	}
	if t.Max != nil {
		s.Maximum = t.Max
	}
	if t.Readonly || t.Immutable {
		s.ReadOnly = true
	}
	if t.WriteOnly {
		s.WriteOnly = true
	}
}
