package maniflex

// Phase 1 / T1.2 PoD: the Deserialize step captures top-level write-body key
// presence (PATCH semantics) and the ?select= projection set onto the transient
// carrier-staging fields, additively — ParsedBody is unaffected.

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func keySet(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestPresentKeysFromJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]struct{}
		err  bool
	}{
		{
			name: "explicit null counts as present, omitted is absent",
			body: `{"name":"Jane","note":null}`,
			want: keySet("name", "note"),
		},
		{
			name: "nested objects contribute only their top-level key",
			body: `{"name":"Jane","address":{"city":"Springfield","zip":"62704"}}`,
			want: keySet("name", "address"),
		},
		{
			name: "empty object yields empty (non-nil) set",
			body: `{}`,
			want: keySet(),
		},
		{
			name: "non-object body is an error",
			body: `[1,2,3]`,
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := presentKeysFromJSON([]byte(tc.body))
			if tc.err {
				if err == nil {
					t.Fatalf("expected error for body %s", tc.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("present = %v, want %v", sortedKeys(got), sortedKeys(tc.want))
			}
		})
	}
}

func TestSelectKeysFromRequest(t *testing.T) {
	cases := []struct {
		query string
		want  map[string]struct{}
	}{
		{"", nil},
		{"select=name,age", keySet("name", "age")},
		{"select=%20name%20,%20,%20age%20", keySet("name", "age")}, // trims + drops empties
		{"page=2", nil}, // unrelated params don't trigger a set
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/?"+tc.query, nil)
		got := selectKeysFromRequest(r)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("query %q: select = %v, want %v", tc.query, sortedKeys(got), sortedKeys(tc.want))
		}
	}
}

// presenceStubRegistry is a no-op registry; the deserialize integration test
// uses a model with no relation includes, so Get is never consulted.
type presenceStubRegistry struct{}

func (presenceStubRegistry) Get(string) (*ModelMeta, bool) { return nil, false }
func (presenceStubRegistry) All() []*ModelMeta             { return nil }

// TestDeserialize_CapturesPresenceAndSelect drives the real Deserialize step and
// asserts the transient carriers are populated while ParsedBody is unchanged.
func TestDeserialize_CapturesPresenceAndSelect(t *testing.T) {
	meta, err := ScanModel(carrierModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	s := newDefaultSteps(nil, presenceStubRegistry{})

	body := `{"name":"Jane","age":34,"note":null}`
	req := httptest.NewRequest("POST", "/?select=name,age", strings.NewReader(body))
	ctx := &ServerContext{Request: req, Model: meta, Operation: OpCreate}

	if err := s.deserialize(ctx, func() error { return nil }); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if ctx.Response != nil {
		t.Fatalf("unexpected abort: %+v", ctx.Response)
	}

	if want := keySet("name", "age", "note"); !reflect.DeepEqual(ctx.present, want) {
		t.Errorf("present = %v, want %v", sortedKeys(ctx.present), sortedKeys(want))
	}
	if want := keySet("name", "age"); !reflect.DeepEqual(ctx.selectKeys, want) {
		t.Errorf("selectKeys = %v, want %v", sortedKeys(ctx.selectKeys), sortedKeys(want))
	}
	// Additive: ParsedBody still carries the decoded values as before.
	if v, _ := ctx.ParsedBody.Get("name"); v != "Jane" {
		t.Errorf("ParsedBody not populated as before: %v", ctx.ParsedBody.Map())
	}
}

// TestParseMultipart_CapturesPresence proves multipart form-value and file-part
// field names are recorded as present write keys.
func TestParseMultipart_CapturesPresence(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("name", "Jane")
	_ = w.WriteField("age", "34")
	part, _ := w.CreateFormFile("avatar", "a.png")
	_, _ = part.Write([]byte("PNGDATA"))
	_ = w.Close()

	req := httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	s := newDefaultSteps(nil, presenceStubRegistry{})
	ctx := &ServerContext{Request: req, Model: nil, Operation: OpCreate}

	if err := s.parseMultipart(ctx); err != nil {
		t.Fatalf("parseMultipart: %v", err)
	}
	if want := keySet("name", "age", "avatar"); !reflect.DeepEqual(ctx.present, want) {
		t.Errorf("present = %v, want %v", sortedKeys(ctx.present), sortedKeys(want))
	}
}
