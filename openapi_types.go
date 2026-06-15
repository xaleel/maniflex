package maniflex

// OpenAPISpec is the root OpenAPI 3.1.0 document.
// It is populated by the Generate pipeline step and serialised to JSON by
// the Response step. Middleware registered on Pipeline.OpenAPI.Generate can
// read and mutate it freely.
type OpenAPISpec struct {
	OpenAPI    string              `json:"openapi"`
	Info       OpenAPIInfo         `json:"info"`
	Servers    []OpenAPIServer     `json:"servers,omitempty"`
	Tags       []OpenAPITag        `json:"tags,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components OpenAPIComponents   `json:"components"`
}

// OpenAPIInfo is the Info Object.
type OpenAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// OpenAPIServer is a Server Object.
type OpenAPIServer struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// OpenAPITag is a Tag Object used for grouping operations in Swagger UI.
type OpenAPITag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PathItem holds all operations for one URL path.
type PathItem struct {
	Get    *OASOperation `json:"get,omitempty"`
	Post   *OASOperation `json:"post,omitempty"`
	Put    *OASOperation `json:"put,omitempty"`
	Patch  *OASOperation `json:"patch,omitempty"`
	Delete *OASOperation `json:"delete,omitempty"`
}

// OASOperation is an Operation Object.
type OASOperation struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description,omitempty"`
	Tags        []string               `json:"tags,omitempty"`
	Deprecated  bool                   `json:"deprecated,omitempty"`
	Parameters  []OASParameter         `json:"parameters,omitempty"`
	RequestBody *OASRequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]OASResponse `json:"responses"`

	// Security lists the security requirement objects for this operation.
	// Each map names a security scheme (declared in components.securitySchemes)
	// mapped to the scopes it requires, e.g. {"bearerAuth": {}}.
	Security []map[string][]string `json:"security,omitempty"`
}

// OASParameter is a Parameter Object (path or query).
type OASParameter struct {
	Name        string     `json:"name"`
	In          string     `json:"in"` // "path" | "query"
	Required    bool       `json:"required,omitempty"`
	Description string     `json:"description,omitempty"`
	Schema      *OASSchema `json:"schema,omitempty"`
	Explode     *bool      `json:"explode,omitempty"`
	Style       string     `json:"style,omitempty"`
}

// OASRequestBody is a Request Body Object.
type OASRequestBody struct {
	Description string                  `json:"description,omitempty"`
	Required    bool                    `json:"required"`
	Content     map[string]OASMediaType `json:"content"`
}

// OASMediaType is a Media Type Object.
type OASMediaType struct {
	Schema   *OASSchema             `json:"schema,omitempty"`
	Encoding map[string]OASEncoding `json:"encoding,omitempty"`
}

// OASEncoding is an Encoding Object, used inside multipart/form-data media
// types to declare per-part content type and other transfer attributes.
type OASEncoding struct {
	ContentType string `json:"contentType,omitempty"`
}

// OASResponse is a Response Object.
type OASResponse struct {
	Description string                  `json:"description"`
	Content     map[string]OASMediaType `json:"content,omitempty"`
}

// OpenAPIComponents holds reusable component objects.
type OpenAPIComponents struct {
	Schemas         map[string]*OASSchema        `json:"schemas,omitempty"`
	SecuritySchemes map[string]OASSecurityScheme `json:"securitySchemes,omitempty"`
}

// OASSecurityScheme is a Security Scheme Object.
type OASSecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	In           string `json:"in,omitempty"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
}

// OASSchema is a JSON Schema 2020-12 / OAS 3.1 schema object.
//
// The Type field is `any` because OAS 3.1 allows both a single string
// ("string") and an array of strings (["string", "null"]) for nullable types.
type OASSchema struct {
	// Core
	Ref         string `json:"$ref,omitempty"`
	Type        any    `json:"type,omitempty"` // string | []string
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`

	// Object
	Properties           map[string]*OASSchema `json:"properties,omitempty"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties *bool                 `json:"additionalProperties,omitempty"`

	// Array
	Items *OASSchema `json:"items,omitempty"`

	// Validation
	Enum      []any    `json:"enum,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
	MinLength *int     `json:"minLength,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`

	// Annotations
	ReadOnly  bool `json:"readOnly,omitempty"`
	WriteOnly bool `json:"writeOnly,omitempty"`

	// Composition
	AllOf []*OASSchema `json:"allOf,omitempty"`
	AnyOf []*OASSchema `json:"anyOf,omitempty"`
	OneOf []*OASSchema `json:"oneOf,omitempty"`
}

// ref returns a $ref schema pointing to a named component schema.
func ref(name string) *OASSchema {
	return &OASSchema{Ref: "#/components/schemas/" + name}
}

// nullable wraps a schema so it also accepts null (OAS 3.1 style).
func nullable(s *OASSchema) *OASSchema {
	if s.Ref != "" {
		return &OASSchema{AnyOf: []*OASSchema{s, {Type: "null"}}}
	}
	switch v := s.Type.(type) {
	case string:
		s.Type = []string{v, "null"}
	}
	return s
}
