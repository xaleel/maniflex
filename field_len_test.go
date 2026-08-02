package maniflex

// VL-1 — min:/max: are numeric bounds and nothing else. Declaring one on a
// string (a natural reading of "max:5000" as a length cap) used to reject every
// non-empty value at runtime with "field must be a number", with nothing at
// startup to catch the mismatch. The framework's own tutorial shipped
// `Password string mfx:"min:8"`, which rejected every password.
//
// min:/max: on a non-numeric field is now a registration error naming the fix,
// and minlen:/maxlen: exist to express what those authors meant.

import (
	"reflect"
	"strings"
	"testing"
)

// ── parsing ───────────────────────────────────────────────────────────────────

// tagsOf parses an mfx tag as it would appear on a field of the given type.
func tagsOf(goType reflect.Type, mfx string) FieldTags {
	return parseFieldTags(reflect.StructField{
		Name: "F", Type: goType,
		Tag: reflect.StructTag(`json:"f" mfx:"` + mfx + `"`),
	})
}

func TestParseLenTags(t *testing.T) {
	tags := tagsOf(reflect.TypeOf(""), "minlen:8,maxlen:5000")
	if tags.MinLen == nil || *tags.MinLen != 8 {
		t.Fatalf("MinLen = %v, want 8", tags.MinLen)
	}
	if tags.MaxLen == nil || *tags.MaxLen != 5000 {
		t.Fatalf("MaxLen = %v, want 5000", tags.MaxLen)
	}
}

// A malformed bound must not be swallowed — that is the whole reason min:'s old
// behaviour was a bug.
func TestParseLenTags_MalformedIsFlagged(t *testing.T) {
	for _, raw := range []string{"minlen:abc", "maxlen:", "maxlen:-1", "minlen:1.5"} {
		tags := tagsOf(reflect.TypeOf(""), raw)
		if len(tags.MalformedOpts) == 0 {
			t.Errorf("%q: want it flagged malformed, got MinLen=%v MaxLen=%v",
				raw, tags.MinLen, tags.MaxLen)
		}
	}
}

// ── registration ──────────────────────────────────────────────────────────────

func scanErr(t *testing.T, v any) string {
	t.Helper()
	_, err := ScanModel(v, ModelConfig{})
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRegistration_NumericBoundOnNonNumericIsRejected(t *testing.T) {
	type StrMin struct {
		BaseModel
		Password string `json:"password" mfx:"min:8"`
	}
	msg := scanErr(t, StrMin{})
	if msg == "" {
		t.Fatal("min: on a string must be a registration error")
	}
	// The message has to carry the fix, or an author just learns that their
	// working-looking tag is banned.
	for _, want := range []string{"Password", "string", "minlen"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q: %s", want, msg)
		}
	}
}

func TestRegistration_NumericBoundRejectedOnOtherKinds(t *testing.T) {
	type SliceMax struct {
		BaseModel
		Tags []string `json:"tags" mfx:"max:5"`
	}
	type BoolMin struct {
		BaseModel
		Flag bool `json:"flag" mfx:"min:0"`
	}
	for name, v := range map[string]any{"slice": SliceMax{}, "bool": BoolMin{}} {
		if scanErr(t, v) == "" {
			t.Errorf("%s: min:/max: must be rejected on a non-numeric field", name)
		}
	}
}

func TestRegistration_NumericBoundOnNumericIsFine(t *testing.T) {
	type Ok struct {
		BaseModel
		Score int     `json:"score" mfx:"min:1,max:10"`
		Rate  float64 `json:"rate"  mfx:"min:0"`
		Opt   *int    `json:"opt"   mfx:"max:5"`
	}
	if msg := scanErr(t, Ok{}); msg != "" {
		t.Fatalf("numeric bounds on numeric fields must be accepted: %s", msg)
	}
}

func TestRegistration_LenBoundOnStringAndSliceIsFine(t *testing.T) {
	type Ok struct {
		BaseModel
		Body string   `json:"body" mfx:"minlen:1,maxlen:5000"`
		Tags []string `json:"tags" mfx:"maxlen:20"`
	}
	if msg := scanErr(t, Ok{}); msg != "" {
		t.Fatalf("length bounds on a string and a slice must be accepted: %s", msg)
	}
}

func TestRegistration_LenBoundOnNumberIsRejected(t *testing.T) {
	type NumLen struct {
		BaseModel
		Score int `json:"score" mfx:"maxlen:10"`
	}
	msg := scanErr(t, NumLen{})
	if msg == "" {
		t.Fatal("maxlen: on an int must be a registration error")
	}
	// The mirror of the other message: point at the tag that does bound a number.
	if !strings.Contains(msg, "max:") {
		t.Errorf("error should point at max: %s", msg)
	}
}

// max_count already bounds a FileKeys field, and is protective (it has a
// default and refuses a malformed value). A second spelling on the same field
// would leave which one applies to reading order.
func TestRegistration_LenBoundOnFileKeysPointsAtMaxCount(t *testing.T) {
	type Doc struct {
		BaseModel
		Files FileKeys `json:"files" mfx:"file,maxlen:5"`
	}
	msg := scanErr(t, Doc{})
	if msg == "" {
		t.Fatal("maxlen: on a FileKeys field must be rejected")
	}
	if !strings.Contains(msg, "max_count") {
		t.Errorf("error should name max_count: %s", msg)
	}
}

func TestRegistration_ImpossibleLenRangeIsRejected(t *testing.T) {
	type Bad struct {
		BaseModel
		Body string `json:"body" mfx:"minlen:10,maxlen:5"`
	}
	if scanErr(t, Bad{}) == "" {
		t.Fatal("minlen greater than maxlen can never be satisfied and must be refused")
	}
}

// ── validation ────────────────────────────────────────────────────────────────

func lenField(goType reflect.Type, mfx string) *FieldMeta {
	return &FieldMeta{Name: "F", Type: goType, Tags: tagsOf(goType, mfx)}
}

func TestCheckFieldValue_StringLength(t *testing.T) {
	f := lenField(reflect.TypeOf(""), "minlen:3,maxlen:5")

	if msg := checkFieldValue(f, "abcd"); msg != "" {
		t.Errorf("a value inside the range must pass, got %q", msg)
	}
	if msg := checkFieldValue(f, "ab"); msg == "" {
		t.Error("a value under minlen must fail")
	}
	if msg := checkFieldValue(f, "abcdef"); msg == "" {
		t.Error("a value over maxlen must fail")
	}
}

// Length is counted in characters, not bytes: otherwise a cap that fits an
// English message rejects the same message in Arabic, which is a bug only some
// users ever see.
func TestCheckFieldValue_StringLengthCountsRunes(t *testing.T) {
	f := lenField(reflect.TypeOf(""), "maxlen:5")

	for _, s := range []string{"hello", "cafés", "😀😀😀😀😀", "مرحبا"} {
		if msg := checkFieldValue(f, s); msg != "" {
			t.Errorf("%q is 5 characters and must pass, got %q", s, msg)
		}
	}
	if msg := checkFieldValue(f, "abcdef"); msg == "" {
		t.Error("6 characters must fail")
	}
}

func TestCheckFieldValue_SliceLength(t *testing.T) {
	f := lenField(reflect.TypeOf([]string{}), "minlen:1,maxlen:3")

	if msg := checkFieldValue(f, []any{"a", "b"}); msg != "" {
		t.Errorf("2 elements must pass, got %q", msg)
	}
	if msg := checkFieldValue(f, []any{}); msg == "" {
		t.Error("0 elements must fail minlen:1")
	}
	if msg := checkFieldValue(f, []any{"a", "b", "c", "d"}); msg == "" {
		t.Error("4 elements must fail maxlen:3")
	}
	// A typed slice reaches the same check through the typed body path.
	if msg := checkFieldValue(f, []string{"a", "b"}); msg != "" {
		t.Errorf("a typed slice must be measured too, got %q", msg)
	}
}

// A bound the value cannot be measured against is a failed bound, not an absent
// one — the rule the numeric path already follows.
func TestCheckFieldValue_UnmeasurableValueFails(t *testing.T) {
	f := lenField(reflect.TypeOf(""), "maxlen:5")
	if msg := checkFieldValue(f, 42); msg == "" {
		t.Error("a length bound on an unmeasurable value must fail, not pass")
	}
}

// ── OpenAPI keywords ──────────────────────────────────────────────────────────

// A length bound documents as different keywords depending on what it bounds.
// The array branch has no model field that reaches it today — a custom SQLTyper
// list is absent from the model schema — so it is covered here directly rather
// than left untested.
func TestApplyFieldValidation_LengthKeywords(t *testing.T) {
	eight, five := 8, 5
	f := FieldMeta{Tags: FieldTags{MinLen: &eight, MaxLen: &five}}

	str := &OASSchema{Type: "string"}
	applyFieldValidation(str, f)
	if str.MinLength == nil || *str.MinLength != 8 || str.MaxLength == nil || *str.MaxLength != 5 {
		t.Errorf("string: want minLength/maxLength, got %+v", str)
	}
	if str.MinItems != nil || str.MaxItems != nil {
		t.Errorf("string: must not carry item bounds, got %+v", str)
	}

	arr := &OASSchema{Type: "array"}
	applyFieldValidation(arr, f)
	if arr.MinItems == nil || *arr.MinItems != 8 || arr.MaxItems == nil || *arr.MaxItems != 5 {
		t.Errorf("array: want minItems/maxItems, got %+v", arr)
	}
	if arr.MinLength != nil || arr.MaxLength != nil {
		t.Errorf("array: must not carry string bounds, got %+v", arr)
	}
}

// A numeric bound still documents as minimum/maximum.
func TestApplyFieldValidation_NumericKeywords(t *testing.T) {
	lo, hi := 1.0, 10.0
	s := &OASSchema{Type: "integer"}
	applyFieldValidation(s, FieldMeta{Tags: FieldTags{Min: &lo, Max: &hi}})
	if s.Minimum == nil || *s.Minimum != 1 || s.Maximum == nil || *s.Maximum != 10 {
		t.Errorf("want minimum/maximum, got %+v", s)
	}
}
