package maniflex

// A nil slice marshals to null, so `[]string` was published as {"type":"array"}
// while the server could send null — the same under-declaration the pointer fix
// closed (todo/ASKS.md issue 3), filed alongside it as still open.
//
// The omitempty asymmetry carries over unchanged: model responses come from
// map[string]any, where the json tag never reaches the encoder, so a nil slice
// is always written as null there. An action body is the struct itself, where
// omitempty leaves an empty slice OUT rather than writing null.
//
// A fixed-size array is excluded on purpose: [4]int cannot be nil.
//
//	go test . -run TestSliceNullability

import (
	"reflect"
	"testing"
)

type sliceBody struct {
	Tags     []string `json:"tags"`
	Optional []string `json:"optional,omitempty"`
	Blob     []byte   `json:"blob"`
	Fixed    [3]int   `json:"fixed"`
}

func TestSliceNullability_ModelPathWrapsASlice(t *testing.T) {
	s := goTypeToSchema(reflect.TypeOf([]string{}))
	if got := typeOf(s); got != "array|null" {
		t.Errorf("[]string = %q, want %q — a nil slice is written as null on the model path", got, "array|null")
	}
}

func TestSliceNullability_ModelPathWrapsAByteSlice(t *testing.T) {
	// A []byte is a base64 string rather than an array, but a nil one is still
	// null, so the nullability is the same question as for any other slice.
	s := goTypeToSchema(reflect.TypeOf([]byte{}))
	if got := typeOf(s); got != "string|null" {
		t.Errorf("[]byte = %q, want %q", got, "string|null")
	}
}

func TestSliceNullability_ModelPathLeavesAFixedArrayAlone(t *testing.T) {
	s := goTypeToSchema(reflect.TypeOf([3]int{}))
	if got := typeOf(s); got != "array" {
		t.Errorf("[3]int = %q, want %q — a fixed-size array cannot be nil", got, "array")
	}
}

func TestSliceNullability_ActionFieldWithoutOmitEmptyIsNullable(t *testing.T) {
	s := reflectTypeSchema(reflect.TypeOf(sliceBody{}), 0)
	if got := typeOf(propOf(t, s, "tags")); got != "array|null" {
		t.Errorf("tags = %q, want %q", got, "array|null")
	}
}

func TestSliceNullability_ActionFieldWithOmitEmptyIsNotNullable(t *testing.T) {
	s := reflectTypeSchema(reflect.TypeOf(sliceBody{}), 0)
	if got := typeOf(propOf(t, s, "optional")); got != "array" {
		t.Errorf("optional = %q, want %q — omitempty leaves an empty slice out rather than writing null, "+
			"so declaring it nullable would promise a null this server never sends", got, "array")
	}
}

func TestSliceNullability_ActionBodyItselfIsNotNullable(t *testing.T) {
	// depth 0 is the body. ActionConfig.RequestSchema documents a slice type as
	// a way to name "an array body", not as "the body may be null" — the same
	// exemption the pointer rule takes.
	s := reflectTypeSchema(reflect.TypeOf([]string{}), 0)
	if got := typeOf(s); got != "array" {
		t.Errorf("body []string = %q, want %q", got, "array")
	}
}

func TestSliceNullability_ActionFixedArrayIsNotNullable(t *testing.T) {
	s := reflectTypeSchema(reflect.TypeOf(sliceBody{}), 0)
	if got := typeOf(propOf(t, s, "fixed")); got != "array" {
		t.Errorf("fixed = %q, want %q", got, "array")
	}
}
