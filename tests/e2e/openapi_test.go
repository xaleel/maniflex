package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestOpenAPI covers the GET /openapi.json endpoint.
func TestOpenAPI(t *testing.T) {
	t.Parallel()

	t.Run("spec_endpoint_returns_200_with_json_content_type", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)
		ct := resp.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("content-type: got %q, want application/json", ct)
		}
	})

	t.Run("spec_is_valid_json", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		var spec map[string]any
		if err := json.Unmarshal(resp.Body, &spec); err != nil {
			t.Fatalf("spec is not valid JSON: %v\nbody: %s", err, resp.Body)
		}
	})

	t.Run("spec_version_is_3_1_0", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "openapi version", body["openapi"], "3.1.0")
		})
	})

	t.Run("spec_has_info_title_and_version", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			info, ok := body["info"].(map[string]any)
			if !ok {
				t.Fatal("spec missing 'info' object")
			}
			if info["title"] == "" {
				t.Error("info.title must not be empty")
			}
			if info["version"] == "" {
				t.Error("info.version must not be empty")
			}
		})
	})

	t.Run("spec_paths_contains_routes_for_all_models", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths, ok := body["paths"].(map[string]any)
			if !ok {
				t.Fatal("spec missing 'paths' object")
			}
			// Default models: User, Post, Comment, Tag → 4 models × 2 paths = 8
			if len(paths) < 8 {
				t.Errorf("expected ≥8 paths for 4 models, got %d: %v",
					len(paths), keysOf(paths))
			}
			// Spot-check specific paths exist
			for _, path := range []string{"/users", "/users/{id}", "/posts", "/posts/{id}"} {
				if _, ok := paths[path]; !ok {
					t.Errorf("expected path %q in spec", path)
				}
			}
		})
	})

	t.Run("spec_has_get_post_on_collection_paths", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			usersPath := paths["/users"].(map[string]any)
			if _, ok := usersPath["get"]; !ok {
				t.Error("/users missing GET operation")
			}
			if _, ok := usersPath["post"]; !ok {
				t.Error("/users missing POST operation")
			}
		})
	})

	t.Run("spec_has_get_patch_delete_on_item_paths", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			itemPath := paths["/users/{id}"].(map[string]any)
			for _, method := range []string{"get", "patch", "delete"} {
				if _, ok := itemPath[method]; !ok {
					t.Errorf("/users/{id} missing %s operation", method)
				}
			}
		})
	})

	t.Run("spec_components_contains_model_schemas", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			components, ok := body["components"].(map[string]any)
			if !ok {
				t.Fatal("spec missing 'components'")
			}
			schemas, ok := components["schemas"].(map[string]any)
			if !ok {
				t.Fatal("components missing 'schemas'")
			}
			// Each model gets 4 schemas: Model, ModelCreate, ModelUpdate, ModelListResponse, ModelResponse
			for _, name := range []string{
				"User", "UserCreate", "UserUpdate",
				"Post", "PostCreate", "PostUpdate",
			} {
				if _, ok := schemas[name]; !ok {
					t.Errorf("missing schema %q in components", name)
				}
			}
		})
	})

	t.Run("create_schema_has_required_fields_listed", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			userCreate := schemas["UserCreate"].(map[string]any)
			required, ok := userCreate["required"].([]any)
			if !ok {
				t.Fatal("UserCreate schema missing 'required' list")
			}
			// name, email, password are required
			reqStrings := anySliceToStrings(required)
			testutil.AssertContains(t, "name required", reqStrings, "name")
			testutil.AssertContains(t, "email required", reqStrings, "email")
			testutil.AssertContains(t, "password required", reqStrings, "password")
		})
	})

	t.Run("update_schema_has_no_required_fields", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			userUpdate := schemas["UserUpdate"].(map[string]any)
			if req, exists := userUpdate["required"]; exists {
				sl, _ := req.([]any)
				if len(sl) > 0 {
					t.Errorf("UserUpdate should have no required fields, got: %v", sl)
				}
			}
		})
	})

	t.Run("response_schema_excludes_writeonly_fields", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			user, ok := schemas["User"].(map[string]any)
			if !ok {
				t.Fatal("User schema not found")
			}
			props, _ := user["properties"].(map[string]any)
			if p, ok := props["password"].(map[string]any); ok {
				if !isTruthy(p["writeOnly"]) {
					t.Error("password in User response schema must be marked writeOnly")
				}
			}
		})
	})

	t.Run("spec_has_tags_for_each_model", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			tags, ok := body["tags"].([]any)
			if !ok {
				t.Fatal("spec missing 'tags'")
			}
			tagNames := make([]string, 0, len(tags))
			for _, tag := range tags {
				m := tag.(map[string]any)
				tagNames = append(tagNames, m["name"].(string))
			}
			for _, model := range []string{"User", "Post", "Comment", "Tag"} {
				testutil.AssertContains(t, "tag for "+model, tagNames, model)
			}
		})
	})

	t.Run("spec_list_operation_has_filter_and_sort_params", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			usersGet := paths["/users"].(map[string]any)["get"].(map[string]any)
			params, _ := usersGet["parameters"].([]any)
			paramNames := make([]string, 0, len(params))
			for _, p := range params {
				m := p.(map[string]any)
				paramNames = append(paramNames, m["name"].(string))
			}
			testutil.AssertContains(t, "page param", paramNames, "page")
			testutil.AssertContains(t, "limit param", paramNames, "limit")
			testutil.AssertContains(t, "filter param", paramNames, "filter")
			testutil.AssertContains(t, "sort param", paramNames, "sort")
		})
	})

	t.Run("spec_item_operation_has_id_path_param", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			usersIDGet := paths["/users/{id}"].(map[string]any)["get"].(map[string]any)
			params, _ := usersIDGet["parameters"].([]any)
			for _, p := range params {
				m := p.(map[string]any)
				if m["name"] == "id" && m["in"] == "path" {
					return
				}
			}
			t.Error("GET /users/{id} missing 'id' path parameter")
		})
	})

	t.Run("spec_allows_cors_header", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		origin := resp.Header.Get("Access-Control-Allow-Origin")
		if origin == "" {
			t.Error("spec endpoint should set Access-Control-Allow-Origin for Swagger UI")
		}
	})

	t.Run("health_endpoint_not_leaked_into_spec_paths", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths, _ := body["paths"].(map[string]any)
			if _, ok := paths["/health"]; ok {
				t.Error("/health must not appear in the OpenAPI spec paths")
			}
		})
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func anySliceToStrings(s []any) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
