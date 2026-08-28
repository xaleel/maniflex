package e2e

// Downstream report (todo/ASKS.md issue 3): a custom action's response schema
// declared *int64 as {"type":"integer"} while the server sent null for a nil
// one. The unit tests in the root package pin the reflector; this pins the
// generated document, which is what a client actually reads — the wiring
// between the two is what the report's evidence was gathered from.
//
//	go test ./tests/e2e/ -run TestActionSpecNullability

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type nullListing struct {
	ID          string  `json:"id"`
	PriceBefore *int64  `json:"price_before"`           // nil -> null
	Discount    *int64  `json:"discount,omitempty"`     // nil -> absent
	Note        *string `json:"note"`                   // nil -> null
	Title       string  `json:"title"`
}

// typeSet renders a schema's "type" as a set, since OAS 3.1 nullability is a
// two-element type array whose order this test should not depend on.
func typeSet(v any) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case string:
		out[t] = true
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func TestActionSpecNullability(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/listings",
				OpenAPI: maniflex.ActionOpenAPI{
					ResponseSchema: nullListing{},
					ResponseStatus: http.StatusOK,
				},
				Handler: func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	body := srv.GET("/openapi.json").Body
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}

	props := actionResponseProps(t, doc, "/listings")

	for _, c := range []struct {
		field string
		want  []string
	}{
		{"price_before", []string{"integer", "null"}},
		{"note", []string{"string", "null"}},
		{"discount", []string{"integer"}}, // omitempty: absent, never null
		{"title", []string{"string"}},
		{"id", []string{"string"}},
	} {
		p, ok := props[c.field].(map[string]any)
		if !ok {
			t.Errorf("%s: missing from the generated response schema", c.field)
			continue
		}
		got := typeSet(p["type"])
		if len(got) != len(c.want) {
			t.Errorf("%s: type = %v, want %v", c.field, p["type"], c.want)
			continue
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: type = %v, want %v", c.field, p["type"], c.want)
				break
			}
		}
	}
}

// actionResponseProps digs the 200 response body properties out of the document
// for the given path's GET.
func actionResponseProps(t *testing.T, doc map[string]any, path string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	if item == nil {
		t.Fatalf("no path %q in the document", path)
	}
	op, _ := item["get"].(map[string]any)
	if op == nil {
		t.Fatalf("no GET on %q", path)
	}
	responses, _ := op["responses"].(map[string]any)
	resp, _ := responses["200"].(map[string]any)
	if resp == nil {
		t.Fatalf("no 200 response on GET %q; have %v", path, responses)
	}
	content, _ := resp["content"].(map[string]any)
	appjson, _ := content["application/json"].(map[string]any)
	schema, _ := appjson["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("no properties on the 200 schema for GET %q: %v", path, schema)
	}
	return props
}
