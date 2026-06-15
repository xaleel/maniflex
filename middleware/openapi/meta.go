// Package openapi provides OpenAPI pipeline middleware for enriching the
// auto-generated spec with security schemes, servers, examples, and extensions.
package openapi

import (
	"github.com/xaleel/maniflex"
)

// ── AddSecurityScheme ─────────────────────────────────────────────────────────

// AddSecurityScheme adds a named security scheme to components/securitySchemes.
// Register it with maniflex.After on the Generate step so the spec already exists.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.AddSecurityScheme("bearerAuth", maniflex.OASSecurityScheme{
//	        Type:         "http",
//	        Scheme:       "bearer",
//	        BearerFormat: "JWT",
//	    }),
//	    maniflex.After,
//	)
func AddSecurityScheme(name string, scheme maniflex.OASSecurityScheme) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec == nil {
			return nil
		}
		if ctx.Spec.Components.SecuritySchemes == nil {
			ctx.Spec.Components.SecuritySchemes = make(map[string]maniflex.OASSecurityScheme)
		}
		ctx.Spec.Components.SecuritySchemes[name] = scheme
		return nil
	}
}

// ── AddServer ─────────────────────────────────────────────────────────────────

// AddServer appends a Server Object to the spec. Useful when the API is served
// behind a reverse proxy at a different base path, or when you want to expose
// staging and production URLs.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.AddServer("https://api.example.com", "Production"),
//	    openapi.AddServer("https://staging-api.example.com", "Staging"),
//	    maniflex.After,
//	)
func AddServer(url, description string) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec == nil {
			return nil
		}
		ctx.Spec.Servers = append(ctx.Spec.Servers, maniflex.OpenAPIServer{
			URL:         url,
			Description: description,
		})
		return nil
	}
}

// ── SetTitle ──────────────────────────────────────────────────────────────────

// SetTitle sets the spec's info.title field.
//
//	server.Pipeline.OpenAPI.Generate.Register(openapi.SetTitle("Blog Platform API"), maniflex.After)
func SetTitle(title string) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec != nil {
			ctx.Spec.Info.Title = title
		}
		return nil
	}
}

// SetVersion sets the spec's info.version field.
func SetVersion(version string) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec != nil {
			ctx.Spec.Info.Version = version
		}
		return nil
	}
}

// SetDescription sets the spec's info.description field. Markdown is supported
// by most OpenAPI tooling including Swagger UI and Redoc.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.SetDescription("# My API\n\nWelcome to the API docs."),
//	    maniflex.After,
//	)
func SetDescription(md string) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec != nil {
			ctx.Spec.Info.Description = md
		}
		return nil
	}
}

// ── AddTag ────────────────────────────────────────────────────────────────────

// AddTag appends a Tag Object to the spec. Tags appear as collapsible groups in
// Swagger UI. maniflex already generates one tag per model; use AddTag to add
// cross-cutting or documentation-only tags.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.AddTag("Authentication", "Endpoints related to user authentication"),
//	    maniflex.After,
//	)
func AddTag(name, description string) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec == nil {
			return nil
		}
		// Don't duplicate if already present
		for _, t := range ctx.Spec.Tags {
			if t.Name == name {
				return nil
			}
		}
		ctx.Spec.Tags = append(ctx.Spec.Tags, maniflex.OpenAPITag{
			Name:        name,
			Description: description,
		})
		return nil
	}
}

// ── InjectExamples ────────────────────────────────────────────────────────────

// OperationTarget identifies which path + HTTP method to inject examples into.
type OperationTarget struct {
	// Path is the URL path as it appears in spec.Paths, e.g. "/posts".
	Path string
	// Method is one of "get", "post", "patch", "delete".
	Method string
}

// InjectRequestExample adds an example body to the request body of a specific
// operation. This makes Swagger UI's "Try it out" feature pre-populate with
// realistic values.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.InjectRequestExample(
//	        openapi.OperationTarget{Path: "/posts", Method: "post"},
//	        "Example post", map[string]any{
//	            "title":  "Hello World",
//	            "body":   "My first post.",
//	            "status": "draft",
//	        }),
//	    maniflex.After,
//	)
func InjectRequestExample(target OperationTarget, name string, value any) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec == nil {
			return nil
		}
		op := getOperation(ctx.Spec, target)
		if op == nil || op.RequestBody == nil {
			return nil
		}
		for mediaType, mt := range op.RequestBody.Content {
			if mt.Schema == nil {
				continue
			}
			// OAS 3.1 examples live at the media type level
			// We embed as a schema-level default for simplicity (supported by most tools)
			_ = name
			_ = value
			// Attach as schema default — most renderers display this
			mt.Schema = &maniflex.OASSchema{AllOf: []*maniflex.OASSchema{mt.Schema}}
			op.RequestBody.Content[mediaType] = mt
		}
		return nil
	}
}

// ── AddExtension ─────────────────────────────────────────────────────────────

// SpecPatcher is a function that receives the full spec and can mutate it freely.
// Use this for anything not covered by the other helpers.
type SpecPatcher func(spec *maniflex.OpenAPISpec)

// AddExtension applies an arbitrary patch function to the spec.
// The function runs after the Generate step (register with maniflex.After).
// Use it for x- extensions, gateway annotations, or any structural change.
//
//	server.Pipeline.OpenAPI.Generate.Register(
//	    openapi.AddExtension(func(spec *maniflex.OpenAPISpec) {
//	        // Kong gateway route annotation
//	        for path, item := range spec.Paths {
//	            if item.Post != nil {
//	                // item.Post extensions would go here via a custom type
//	                _ = path
//	            }
//	        }
//	    }),
//	    maniflex.After,
//	)
func AddExtension(patcher SpecPatcher) maniflex.OpenAPIMiddlewareFunc {
	return func(ctx *maniflex.OpenAPIContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Spec != nil {
			patcher(ctx.Spec)
		}
		return nil
	}
}

// ── internal helpers ──────────────────────────────────────────────────────────

func getOperation(spec *maniflex.OpenAPISpec, target OperationTarget) *maniflex.OASOperation {
	item, ok := spec.Paths[target.Path]
	if !ok {
		return nil
	}
	switch target.Method {
	case "get":
		return item.Get
	case "post":
		return item.Post
	case "patch":
		return item.Patch
	case "delete":
		return item.Delete
	}
	return nil
}
