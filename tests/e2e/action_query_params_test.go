package e2e

// action_query_params_test.go pins how ActionConfig.OpenAPI.QueryParams reaches
// the generated document (GAP-13). TestActionOpenAPIBlock already checks that a
// declared parameter's *name* shows up; these tests assert the emitted parameter
// objects themselves, because a downstream client generator reads the whole
// object — an operation whose parameter is present but malformed is no more
// usable than one with no parameter at all.
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestActionQueryParams

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestActionQueryParams(t *testing.T) {
	t.Parallel()

	t.Run("declared_parameter_is_emitted_in_full", func(t *testing.T) {
		// Every declared attribute has to survive to the document: a generator
		// that sees the name but no schema cannot type the argument, and one
		// that sees no `required` will make a mandatory parameter optional.
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/{id}/export",
			maniflex.OASParameter{
				Name:        "format",
				In:          "query",
				Required:    true,
				Description: "Output format.",
				Schema:      &maniflex.OASSchema{Type: "string", Enum: []any{"csv", "xlsx"}},
			})

		p := actionParam(t, srv, "/reports/{id}/export", "get", "format")
		testutil.AssertEqual(t, "in", p["in"], "query")
		testutil.AssertEqual(t, "required", p["required"], true)
		testutil.AssertEqual(t, "description", p["description"], "Output format.")

		schema, ok := p["schema"].(map[string]any)
		if !ok {
			t.Fatalf("parameter has no schema: %v", p)
		}
		testutil.AssertEqual(t, "schema type", schema["type"], "string")
		enum := asAnySlice(schema["enum"])
		if len(enum) != 2 || enum[0] != "csv" || enum[1] != "xlsx" {
			t.Errorf("schema enum: got %v, want [csv xlsx]", schema["enum"])
		}
	})

	t.Run("optional_parameter_omits_required", func(t *testing.T) {
		// OpenAPI treats an absent `required` as false, and OASParameter tags it
		// omitempty. Emitting `"required": false` would be equivalent but noisier;
		// this pins which of the two we produce.
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/{id}/export",
			maniflex.OASParameter{
				Name:   "notify",
				In:     "query",
				Schema: &maniflex.OASSchema{Type: "boolean"},
			})

		p := actionParam(t, srv, "/reports/{id}/export", "get", "notify")
		if _, present := p["required"]; present {
			t.Errorf("an optional parameter should omit `required`, got %v", p)
		}
	})

	t.Run("path_params_come_first_then_declared_order", func(t *testing.T) {
		// Path parameters are extracted from the route and the declared ones are
		// appended, in the order given. Parameter order is not semantically
		// meaningful in OpenAPI, but it is what a generated client's positional
		// arguments and the rendered docs follow, so it should be predictable.
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/{id}/export",
			maniflex.OASParameter{Name: "format", In: "query",
				Schema: &maniflex.OASSchema{Type: "string"}},
			maniflex.OASParameter{Name: "since", In: "query",
				Schema: &maniflex.OASSchema{Type: "string", Format: "date-time"}},
			maniflex.OASParameter{Name: "notify", In: "query",
				Schema: &maniflex.OASSchema{Type: "boolean"}},
		)

		op := actionOp(t, srv, "/reports/{id}/export", "get")
		got := paramNamesOf(op)
		want := []string{"id", "format", "since", "notify"}
		if len(got) != len(want) {
			t.Fatalf("parameters: got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("parameters: got %v, want %v", got, want)
			}
		}
	})

	t.Run("auto_extracted_path_param_is_required_and_typed", func(t *testing.T) {
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/{id}/export",
			maniflex.OASParameter{Name: "format", In: "query",
				Schema: &maniflex.OASSchema{Type: "string"}})

		p := actionParam(t, srv, "/reports/{id}/export", "get", "id")
		testutil.AssertEqual(t, "in", p["in"], "path")
		testutil.AssertEqual(t, "required", p["required"], true)
		schema, _ := p["schema"].(map[string]any)
		testutil.AssertEqual(t, "schema type", schema["type"], "string")
	})

	t.Run("an_action_without_path_params_emits_only_its_query_params", func(t *testing.T) {
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/summary",
			maniflex.OASParameter{Name: "group_by", In: "query",
				Schema: &maniflex.OASSchema{Type: "string"}})

		op := actionOp(t, srv, "/reports/summary", "get")
		got := paramNamesOf(op)
		if len(got) != 1 || got[0] != "group_by" {
			t.Errorf("parameters: got %v, want [group_by]", got)
		}
	})

	t.Run("two_methods_on_one_path_keep_their_own_parameters", func(t *testing.T) {
		// Both operations live in the same path item, and the generator writes
		// each one's parameters onto its own operation rather than the shared
		// item — so one action's declaration must not leak into the other's.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}},
			Config: func(cfg *maniflex.Config) { cfg.Documentation.Public = true },
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "GET", Path: "/reports/summary", AllowPublic: true,
					Handler: noopAction,
					OpenAPI: maniflex.ActionOpenAPI{QueryParams: []maniflex.OASParameter{{
						Name: "group_by", In: "query",
						Schema: &maniflex.OASSchema{Type: "string"},
					}}},
				})
				s.Action(maniflex.ActionConfig{
					Method: "POST", Path: "/reports/summary", AllowPublic: true,
					Handler: noopAction,
					OpenAPI: maniflex.ActionOpenAPI{QueryParams: []maniflex.OASParameter{{
						Name: "dry_run", In: "query",
						Schema: &maniflex.OASSchema{Type: "boolean"},
					}}},
				})
			},
		})

		get := paramNamesOf(actionOp(t, srv, "/reports/summary", "get"))
		post := paramNamesOf(actionOp(t, srv, "/reports/summary", "post"))
		if len(get) != 1 || get[0] != "group_by" {
			t.Errorf("GET parameters: got %v, want [group_by]", get)
		}
		if len(post) != 1 || post[0] != "dry_run" {
			t.Errorf("POST parameters: got %v, want [dry_run]", post)
		}
	})

	t.Run("omitted_in_defaults_to_query", func(t *testing.T) {
		// `in` is required by OpenAPI, and a field named QueryParams has exactly
		// one sensible answer for it. Emitting the empty string instead put an
		// invalid document in front of every consumer, from one omitted field
		// that nothing warned about.
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/summary",
			maniflex.OASParameter{
				Name:   "notify",
				Schema: &maniflex.OASSchema{Type: "boolean"},
			})

		p := actionParam(t, srv, "/reports/summary", "get", "notify")
		testutil.AssertEqual(t, "in", p["in"], "query")
	})

	t.Run("explicit_in_is_left_alone", func(t *testing.T) {
		// The default fills a gap; it must not overwrite a header or cookie
		// parameter documented through the same list.
		t.Parallel()
		srv := actionParamServer(t, "GET", "/reports/summary",
			maniflex.OASParameter{
				Name:   "X-Tenant",
				In:     "header",
				Schema: &maniflex.OASSchema{Type: "string"},
			},
			maniflex.OASParameter{
				Name:   "session",
				In:     "cookie",
				Schema: &maniflex.OASSchema{Type: "string"},
			})

		header := actionParam(t, srv, "/reports/summary", "get", "X-Tenant")
		testutil.AssertEqual(t, "header param in", header["in"], "header")
		cookie := actionParam(t, srv, "/reports/summary", "get", "session")
		testutil.AssertEqual(t, "cookie param in", cookie["in"], "cookie")
	})

	t.Run("defaulting_does_not_mutate_the_caller_declaration", func(t *testing.T) {
		// The generator runs per request for a document served over HTTP, and
		// the ActionConfig it reads is shared. Writing the default back into the
		// caller's slice would be a data race between concurrent spec requests.
		t.Parallel()
		declared := []maniflex.OASParameter{{
			Name:   "notify",
			Schema: &maniflex.OASSchema{Type: "boolean"},
		}}
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}},
			Config: func(cfg *maniflex.Config) { cfg.Documentation.Public = true },
			Middleware: func(s *maniflex.Server) {
				s.Action(maniflex.ActionConfig{
					Method: "GET", Path: "/reports/summary", AllowPublic: true,
					Handler: noopAction,
					OpenAPI: maniflex.ActionOpenAPI{QueryParams: declared},
				})
			},
		})

		srv.GET("/openapi.json").AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "caller's In after generation", declared[0].In, "")
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func noopAction(*maniflex.ServerContext) error { return nil }

// actionParamServer registers one public action carrying the given query
// parameter declarations, with the generated documentation published.
func actionParamServer(t *testing.T, method, path string, params ...maniflex.OASParameter) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Config: func(cfg *maniflex.Config) { cfg.Documentation.Public = true },
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:      method,
				Path:        path,
				AllowPublic: true,
				Handler:     noopAction,
				OpenAPI:     maniflex.ActionOpenAPI{QueryParams: params},
			})
		},
	})
}

// actionOp returns one operation object from the generated document.
func actionOp(t *testing.T, srv *testutil.Server, specPath, method string) map[string]any {
	t.Helper()
	resp := srv.GET("/openapi.json")
	resp.AssertStatus(http.StatusOK)

	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)
	item, ok := paths[specPath].(map[string]any)
	if !ok {
		t.Fatalf("spec has no path %q, have: %v", specPath, keys(paths))
	}
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("path %q has no %s operation", specPath, method)
	}
	return op
}

// actionParam returns one named parameter object from an operation.
func actionParam(t *testing.T, srv *testutil.Server, specPath, method, name string) map[string]any {
	t.Helper()
	op := actionOp(t, srv, specPath, method)
	for _, raw := range asAnySlice(op["parameters"]) {
		p, _ := raw.(map[string]any)
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("%s %s declares no %q parameter, has: %v", method, specPath, name, paramNamesOf(op))
	return nil
}
