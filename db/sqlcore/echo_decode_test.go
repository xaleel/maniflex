package sqlcore

import (
	"reflect"
	"testing"

	"github.com/xaleel/maniflex"
)

// decodeStructuredColumns must turn a stored JSON-text LocaleString column into
// a typed maniflex.LocaleString (so it serialises as an object on the
// create/update echo) while leaving scalar columns, framework-internal columns
// with no model field, and GoType-less synthetic models untouched.
func TestDecodeStructuredColumns(t *testing.T) {
	type doc struct{ maniflex.BaseModel }
	model := &maniflex.ModelMeta{
		Name:   "Doc",
		GoType: reflect.TypeOf(doc{}),
		Fields: []maniflex.FieldMeta{
			{Name: "Title", Type: reflect.TypeOf(""), Tags: maniflex.FieldTags{DBName: "title"}},
			{Name: "Label", Type: reflect.TypeOf(maniflex.LocaleString(nil)), Tags: maniflex.FieldTags{DBName: "label"}},
		},
	}
	m := map[string]any{
		"title":      "hello",                    // scalar string — untouched
		"label":      `{"en":"Hi","ar":"مرحبا"}`, // JSON text — decode to LocaleString
		"label_hmac": "deadbeef",                 // no model field — untouched
	}
	decodeStructuredColumns(model, m)

	ls, ok := m["label"].(maniflex.LocaleString)
	if !ok {
		t.Fatalf("label should decode to LocaleString, got %T (%v)", m["label"], m["label"])
	}
	if ls["en"] != "Hi" || ls["ar"] != "مرحبا" {
		t.Fatalf("label decoded wrong: %#v", ls)
	}
	if m["title"] != "hello" {
		t.Fatalf("scalar column must be untouched, got %v", m["title"])
	}
	if m["label_hmac"] != "deadbeef" {
		t.Fatalf("unknown column must be untouched, got %v", m["label_hmac"])
	}

	// Synthetic model (GoType nil) is a strict no-op — its map IS the record.
	syn := &maniflex.ModelMeta{Name: "Syn"}
	sm := map[string]any{"label": `{"en":"x"}`}
	decodeStructuredColumns(syn, sm)
	if sm["label"] != `{"en":"x"}` {
		t.Fatalf("synthetic model must be untouched, got %v", sm["label"])
	}
}
