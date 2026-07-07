package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── 10.7: dangling $ref for unregistered relation targets ─────────────────────

// P10Category is a registered model that P10Item references via an explicit
// mfx:"relation" tag on P10CategoryID (target inferred by stripping "ID").
type P10Category struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// P10Item has two explicit relations: P10CategoryID → P10Category (registered)
// and RelatedID → Related (NOT registered, no companion field). The latter must
// not produce a `related` property whose $ref points at a non-existent schema.
// (Since v0.1.3 relations are opt-in via mfx:"relation" — no longer inferred
// from the ID suffix — so both FKs carry the tag explicitly.)
type P10Item struct {
	maniflex.BaseModel
	Name          string `json:"name"`
	P10CategoryID string `json:"p10_category_id" mfx:"relation"`
	RelatedID     string `json:"related_id" mfx:"relation"`
}

// TestOpenAPI_NoDanglingRelationRef verifies that a relation to an unregistered
// model is omitted from the response schema (10.7), while a relation to a
// registered model is still embedded.
func TestOpenAPI_NoDanglingRelationRef(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{P10Item{}, P10Category{}},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/openapi.json"), nil)
	resp.AssertStatus(http.StatusOK)

	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)

	// The unregistered "Related" model must not appear as a component schema.
	if _, ok := schemas["Related"]; ok {
		t.Error("unregistered relation target 'Related' should not have a component schema")
	}

	item, ok := schemas["P10Item"].(map[string]any)
	if !ok {
		t.Fatalf("P10Item schema missing; have: %v", keysOf(schemas))
	}
	props, _ := item["properties"].(map[string]any)

	// The dangling relation property must be omitted entirely.
	if _, ok := props["related"]; ok {
		t.Error("dangling relation 'related' should be omitted from the response schema")
	}
	// The FK scalar columns themselves are still documented.
	if _, ok := props["related_id"]; !ok {
		t.Error("related_id scalar column should still be present")
	}
	if _, ok := props["p10_category_id"]; !ok {
		t.Error("p10_category_id scalar column should still be present")
	}
	// The relation to the registered P10Category model is still embedded
	// (some property's schema must $ref it).
	embedded := false
	for _, v := range props {
		if m, ok := v.(map[string]any); ok && refsModel(m, "P10Category") {
			embedded = true
			break
		}
	}
	if !embedded {
		t.Error("relation to registered P10Category should still be embedded")
	}

	// Belt-and-braces: no $ref anywhere in the spec points at a missing schema.
	assertNoDanglingRefs(t, spec, schemas)
}

// ── 10.8: action OpenAPI block with struct schema inference ────────────────────

type p10RescheduleReq struct {
	NewTime string `json:"new_time" mfx:"required"`
	Notify  bool   `json:"notify"`
	Reason  string `json:"reason"`
}

type p10RescheduleResp struct {
	ID     string `json:"id"`
	Status string `json:"status" mfx:"enum:scheduled|cancelled"`
}

// TestActionOpenAPIBlock verifies that ActionOpenAPI infers request/response
// schemas from Go structs and folds in query params, security, and description.
func TestActionOpenAPIBlock(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:  "POST",
				Path:    "/appointments/{id}/reschedule",
				Summary: "Reschedule an appointment",
				OpenAPI: maniflex.ActionOpenAPI{
					Description:    "Moves an appointment to a new time.",
					RequestSchema:  p10RescheduleReq{},
					ResponseSchema: p10RescheduleResp{},
					ResponseStatus: http.StatusOK,
					QueryParams: []maniflex.OASParameter{{
						Name: "dry_run", In: "query",
						Schema: &maniflex.OASSchema{Type: "boolean"},
					}},
					Security: []map[string][]string{{"bearerAuth": {}}},
				},
				Handler: func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	post := getActionPost(t, srv, "/appointments/{id}/reschedule")

	if post["description"] != "Moves an appointment to a new time." {
		t.Errorf("description: got %v", post["description"])
	}

	// Request body schema inferred from the struct.
	reqProps, reqRequired := bodyProps(t, post, "requestBody")
	for _, f := range []string{"new_time", "notify", "reason"} {
		if _, ok := reqProps[f]; !ok {
			t.Errorf("request schema missing property %q", f)
		}
	}
	testutil.AssertContains(t, "new_time required", reqRequired, "new_time")

	// Response (200) schema inferred from the struct, with enum carried over.
	resp200 := post["responses"].(map[string]any)["200"].(map[string]any)
	respSchema := resp200["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	respProps := respSchema["properties"].(map[string]any)
	for _, f := range []string{"id", "status"} {
		if _, ok := respProps[f]; !ok {
			t.Errorf("response schema missing property %q", f)
		}
	}
	statusEnum, _ := respProps["status"].(map[string]any)["enum"].([]any)
	if len(statusEnum) != 2 {
		t.Errorf("status enum should have 2 values, got %v", statusEnum)
	}

	// Parameters: auto-extracted path param + declared query param.
	paramNames := paramNamesOf(post)
	testutil.AssertContains(t, "path param id", paramNames, "id")
	testutil.AssertContains(t, "query param dry_run", paramNames, "dry_run")

	// Security requirement folded in.
	if _, ok := post["security"].([]any); !ok {
		t.Errorf("expected security on operation, got %v", post["security"])
	}
}

// TestActionOpenAPIInlineWins verifies the inline RequestBody / Responses fields
// take precedence over the struct-inferred OpenAPI block (back-compat).
func TestActionOpenAPIInlineWins(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/things/{id}/do",
				RequestBody: maniflex.JSONRequestBody(&maniflex.OASSchema{
					Type: "object",
					Properties: map[string]*maniflex.OASSchema{
						"inline_only": {Type: "string"},
					},
				}),
				OpenAPI: maniflex.ActionOpenAPI{
					RequestSchema: p10RescheduleReq{}, // must be ignored
				},
				Handler: func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	post := getActionPost(t, srv, "/things/{id}/do")
	reqProps, _ := bodyProps(t, post, "requestBody")
	if _, ok := reqProps["inline_only"]; !ok {
		t.Error("inline RequestBody should win over OpenAPI.RequestSchema")
	}
	if _, ok := reqProps["new_time"]; ok {
		t.Error("OpenAPI.RequestSchema should be ignored when RequestBody is set")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func getActionPost(t *testing.T, srv *testutil.Server, specPath string) map[string]any {
	t.Helper()
	resp := srv.Do(http.MethodGet, srv.APIPath("/openapi.json"), nil)
	resp.AssertStatus(http.StatusOK)
	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	paths := spec["paths"].(map[string]any)
	item, ok := paths[specPath].(map[string]any)
	if !ok {
		t.Fatalf("expected path %q in spec, have: %v", specPath, keys(paths))
	}
	post, ok := item["post"].(map[string]any)
	if !ok {
		t.Fatalf("expected post operation at %q", specPath)
	}
	return post
}

func bodyProps(t *testing.T, op map[string]any, key string) (map[string]any, []string) {
	t.Helper()
	body, ok := op[key].(map[string]any)
	if !ok {
		t.Fatalf("operation missing %q", key)
	}
	schema := body["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	required := anySliceToStrings(asAnySlice(schema["required"]))
	return props, required
}

func paramNamesOf(op map[string]any) []string {
	params, _ := op["parameters"].([]any)
	out := make([]string, 0, len(params))
	for _, p := range params {
		if m, ok := p.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func asAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func refsModel(schema map[string]any, model string) bool {
	want := "#/components/schemas/" + model
	if schema["$ref"] == want {
		return true
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		for _, sub := range asAnySlice(schema[key]) {
			if m, ok := sub.(map[string]any); ok && m["$ref"] == want {
				return true
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return refsModel(items, model)
	}
	return false
}

// assertNoDanglingRefs walks the whole spec and fails if any $ref points at a
// component schema that was never defined.
func assertNoDanglingRefs(t *testing.T, node any, schemas map[string]any) {
	t.Helper()
	const prefix = "#/components/schemas/"
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "$ref" {
				if ref, ok := val.(string); ok && len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
					name := ref[len(prefix):]
					if _, ok := schemas[name]; !ok {
						t.Errorf("dangling $ref to missing schema %q", name)
					}
				}
				continue
			}
			assertNoDanglingRefs(t, val, schemas)
		}
	case []any:
		for _, item := range v {
			assertNoDanglingRefs(t, item, schemas)
		}
	}
}
