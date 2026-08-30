package maniflex

// Downstream report (todo/ASKS.md issue 3): every custom-action request/response
// schema under-declared nullability. The model path records that a field was a
// pointer and wraps the schema with nullable(); the action path stripped the
// pointer on reflectTypeSchema's first line and never recorded it, so *int64
// came out as {"type":"integer"} while the server sent null.
//
// The omitempty half is not a detail. The two paths have different serialisers:
// model responses are built from map[string]any, where a json omitempty tag is
// inert and a nil value emits "key": null, so wrapping every pointer is right
// there. Action bodies are the Go struct itself, where omitempty means the field
// is ABSENT rather than null — so the action path must suppress the wrap for
// those, and the two paths are meant to differ here.
//
//	go test . -run TestActionSchemaNullability

import (
	"reflect"
	"testing"
	"time"
)

// typeOf renders OASSchema.Type for assertions: "integer", or "integer|null".
func typeOf(s *OASSchema) string {
	if s == nil {
		return "<nil schema>"
	}
	switch v := s.Type.(type) {
	case string:
		return v
	case []string:
		out := ""
		for i, t := range v {
			if i > 0 {
				out += "|"
			}
			out += t
		}
		return out
	default:
		return "<no type>"
	}
}

func propOf(t *testing.T, s *OASSchema, name string) *OASSchema {
	t.Helper()
	if s == nil || s.Properties == nil {
		t.Fatalf("no properties on schema for %q", name)
	}
	p, ok := s.Properties[name]
	if !ok {
		t.Fatalf("no property %q; have %v", name, schemaKeys(s.Properties))
	}
	return p
}

func schemaKeys(m map[string]*OASSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type nullBody struct {
	Price    *int64     `json:"price"`
	PriceOpt *int64     `json:"price_opt,omitempty"`
	Name     *string    `json:"name"`
	NameOpt  *string    `json:"name_opt,omitempty"`
	Flag     *bool      `json:"flag"`
	Ratio    *float64   `json:"ratio"`
	When     *time.Time `json:"when"`
	Nested   *nullLeaf  `json:"nested"`
	Plain    int64      `json:"plain"`
	PlainStr string     `json:"plain_str"`
	List     []*int64   `json:"list"`
	PtrList  *[]string  `json:"ptr_list"`
}

type nullLeaf struct {
	Inner    *int64 `json:"inner"`
	InnerOpt *int64 `json:"inner_opt,omitempty"`
}

// A pointer field without omitempty serialises as null when nil, so the schema
// has to say so — this is the reported bug.
func TestActionSchemaNullability_PointerFieldsAreNullable(t *testing.T) {
	s := reflectSchema(nullBody{})
	for _, c := range []struct{ field, want string }{
		{"price", "integer|null"},
		{"name", "string|null"},
		{"flag", "boolean|null"},
		{"ratio", "number|null"},
		{"when", "string|null"},
		{"nested", "object|null"},
		{"ptr_list", "array|null"},
	} {
		if got := typeOf(propOf(t, s, c.field)); got != c.want {
			t.Errorf("%s: type = %q, want %q — the server sends null for a nil pointer and "+
				"the spec denies it can", c.field, got, c.want)
		}
	}
}

// omitempty means absent, not null. Declaring these nullable tells clients to
// handle a null the server never sends — the opposite error, equally wrong.
func TestActionSchemaNullability_OmitemptyPointersAreNotNullable(t *testing.T) {
	s := reflectSchema(nullBody{})
	for _, c := range []struct{ field, want string }{
		{"price_opt", "integer"},
		{"name_opt", "string"},
	} {
		if got := typeOf(propOf(t, s, c.field)); got != c.want {
			t.Errorf("%s: type = %q, want %q — a nil omitempty pointer is omitted, never "+
				"serialised as null", c.field, got, c.want)
		}
	}
}

// Non-pointer fields must be untouched, or the fix has just made everything
// nullable and says nothing.
func TestActionSchemaNullability_NonPointersUnchanged(t *testing.T) {
	s := reflectSchema(nullBody{})
	for _, c := range []struct{ field, want string }{
		{"plain", "integer"},
		{"plain_str", "string"},
		// A slice is nullable, but for its own reason rather than this one: a
		// nil slice marshals to null. See TestSliceNullability.
		{"list", "array|null"},
	} {
		if got := typeOf(propOf(t, s, c.field)); got != c.want {
			t.Errorf("%s: type = %q, want %q", c.field, got, c.want)
		}
	}
}

// The rule has to hold at every level, not just the outermost struct.
func TestActionSchemaNullability_AppliesInsideNestedStructsAndSlices(t *testing.T) {
	s := reflectSchema(nullBody{})

	nested := propOf(t, s, "nested")
	if got := typeOf(propOf(t, nested, "inner")); got != "integer|null" {
		t.Errorf("nested.inner: type = %q, want %q", got, "integer|null")
	}
	if got := typeOf(propOf(t, nested, "inner_opt")); got != "integer" {
		t.Errorf("nested.inner_opt: type = %q, want %q", got, "integer")
	}

	list := propOf(t, s, "list")
	if list.Items == nil {
		t.Fatal("list has no items schema")
	}
	if got := typeOf(list.Items); got != "integer|null" {
		t.Errorf("list items: type = %q, want %q — a []*int64 carries nulls", got, "integer|null")
	}
}

// A pointer at the top is how ActionConfig.RequestSchema documents "this type"
// — its doc offers value, pointer and reflect.Type interchangeably. It is not a
// claim that the whole body may be null.
func TestActionSchemaNullability_TopLevelPointerIsNotNullable(t *testing.T) {
	byValue := reflectSchema(nullBody{})
	byPointer := reflectSchema(&nullBody{})
	byType := reflectSchema(reflect.TypeOf(&nullBody{}))

	for name, s := range map[string]*OASSchema{"pointer": byPointer, "reflect.Type": byType} {
		if got := typeOf(s); got != typeOf(byValue) {
			t.Errorf("declaring the body by %s gave type %q where the value form gave %q: "+
				"passing a pointer names the type, it does not say the body may be null",
				name, got, typeOf(byValue))
		}
		if len(s.Properties) != len(byValue.Properties) {
			t.Errorf("declaring the body by %s produced %d properties, value form gave %d",
				name, len(s.Properties), len(byValue.Properties))
		}
	}
}

// The model path was already right and must stay right: it serialises from a
// map, so omitempty is inert there and every pointer really can be null.
func TestActionSchemaNullability_ModelPathUnchanged(t *testing.T) {
	rt := reflect.TypeOf(nullBody{})
	for _, c := range []struct{ field, want string }{
		{"Price", "integer|null"},
		{"PriceOpt", "integer|null"}, // omitempty is inert on the model path
		{"Name", "string|null"},
		{"When", "string|null"},
		{"Plain", "integer"},
	} {
		f, ok := rt.FieldByName(c.field)
		if !ok {
			t.Fatalf("no field %s", c.field)
		}
		if got := typeOf(goTypeToSchema(f.Type)); got != c.want {
			t.Errorf("model path %s: type = %q, want %q", c.field, got, c.want)
		}
	}
}
