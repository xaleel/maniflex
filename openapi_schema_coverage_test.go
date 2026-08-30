package maniflex

// A model field whose Go type the schema generator does not recognise used to be
// dropped from the generated spec with no signal at all — the `continue` at the
// top of buildModelSchemas, taken on a nil schema (downstream report, ASKS.md
// issue 2).
//
// Two things were wrong with that, and only one of them is about slices.
//
// The field vanished from the published contract, so a generated client had no
// type for it. Worse, the skip happened before the required list was built, so a
// field tagged mfx:"required" disappeared from `required` too — the spec then
// said a field the server demands does not exist, and a client generated from it
// would POST without it and take a 422 the spec called impossible.
//
// Adding a case for slices fixes the reported instance. It does not fix the
// class: the next type nobody thought of drops just as quietly. So no field is
// omitted any more — one with no inferable schema is published unconstrained —
// and the model is reported at startup, fatally under Config.Strict.

import (
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// scMoney is the realistic case that survives every type-inference rule: a
// struct with its own MarshalJSON, whose serialised shape nothing can read off
// the Go type. The author has to say.
type scMoney struct{ Cents int64 }

func (scMoney) SQLType(DriverType) string      { return "NUMERIC(12,2)" }
func (m scMoney) Value() (driver.Value, error) { return m.Cents, nil }
func (m *scMoney) Scan(any) error              { return nil }
func (m scMoney) MarshalJSON() ([]byte, error) { return json.Marshal(m.Cents) }

type scTags []string

func (scTags) SQLType(DriverType) string      { return "TEXT" }
func (t scTags) Value() (driver.Value, error) { return "[]", nil }
func (t *scTags) Scan(any) error              { return nil }

type scSeal []byte

func (scSeal) SQLType(DriverType) string      { return "TEXT" }
func (s scSeal) Value() (driver.Value, error) { return []byte(s), nil }
func (s *scSeal) Scan(any) error              { return nil }

type scModel struct {
	BaseModel
	Name     string    `json:"name"`
	Tags     scTags    `json:"tags"`
	Needed   scTags    `json:"needed" mfx:"required"`
	Seal     scSeal    `json:"seal"`
	Price    scMoney   `json:"price"`
	Fee      scMoney   `json:"fee"      mfx:"required"`
	Occurred time.Time `json:"occurred"`
}

func scSpec(t *testing.T, model any) (*OpenAPISpec, string) {
	t.Helper()
	s := New(Config{DisableAutoMigrate: true})
	if err := s.Register(model); err != nil {
		t.Fatalf("register: %v", err)
	}
	name := ""
	for _, m := range s.Registry().All() {
		name = m.Name
	}
	return GenerateSpec(s.Registry(), &Config{}, nil), name
}

func scProps(t *testing.T, spec *OpenAPISpec, schema string) map[string]*OASSchema {
	t.Helper()
	sc := spec.Components.Schemas[schema]
	if sc == nil {
		t.Fatalf("no %q schema in the generated spec", schema)
	}
	return sc.Properties
}

func TestOpenAPISchemaIncludesSliceFields(t *testing.T) {
	spec, name := scSpec(t, scModel{})
	props := scProps(t, spec, name)

	tags, ok := props["tags"]
	if !ok {
		t.Fatal("a slice-typed field is missing from the response schema")
	}
	if got := typeOf(tags); got != "array|null" {
		t.Errorf("tags has type %q, want %q — present as an array, and nullable because a nil "+
			"slice marshals to null", got, "array|null")
	}
	if tags.Items == nil || tags.Items.Type != "string" {
		t.Errorf("tags items = %+v, want a string schema", tags.Items)
	}
}

// A byte slice marshals to a base64 string, not to an array of numbers. It is
// the one slice whose JSON shape is not an array, and rendering it as one is the
// mistake the existing reflectTypeSchema makes — so the fix must not copy it.
func TestOpenAPISchemaRendersByteSlicesAsBase64Strings(t *testing.T) {
	spec, name := scSpec(t, scModel{})
	seal, ok := scProps(t, spec, name)["seal"]
	if !ok {
		t.Fatal("a byte-slice field is missing from the response schema")
	}
	// The nullability is a separate question from the base64 shape: a nil byte
	// slice is null, and a non-nil empty one is "". What matters here is that it
	// is a string rather than an array.
	if got := typeOf(seal); got != "string|null" || seal.Format != "byte" {
		t.Errorf("seal = {type:%q format:%q}, want {type:string|null format:byte} — "+
			"encoding/json base64s it, so an array of integers describes a payload "+
			"no server ever sends", got, seal.Format)
	}
}

// reflectTypeSchema serves action request/response bodies, and has the same bug
// today. Fixing only the model path would leave the two disagreeing about one
// Go type, which is how the divergence started.
func TestReflectTypeSchemaRendersByteSlicesAsBase64Strings(t *testing.T) {
	type body struct {
		Blob []byte `json:"blob"`
	}
	s := reflectSchema(body{})
	if s == nil || s.Properties["blob"] == nil {
		t.Fatalf("no blob property: %+v", s)
	}
	blob := s.Properties["blob"]
	if got := typeOf(blob); got != "string|null" || blob.Format != "byte" {
		t.Errorf("blob = {type:%q format:%q}, want {type:string|null format:byte}", got, blob.Format)
	}
}

// The sharper half of the bug: the skip ran before the required list was built.
func TestOpenAPIRequiredSurvivesAnUninferableField(t *testing.T) {
	spec, name := scSpec(t, scModel{})
	create := spec.Components.Schemas[name+"Create"]
	if create == nil {
		t.Fatal("no create schema")
	}
	// "fee" is the case that matters: a required field whose schema cannot be
	// inferred. Before the fix it was skipped before the required list was built,
	// so it was absent from both. "needed" is a required slice — inferable now,
	// and dropped before the slice case existed.
	for _, want := range []string{"needed", "fee"} {
		if _, ok := create.Properties[want]; !ok {
			t.Errorf("required field %q is absent from the create schema", want)
		}
		if !containsString(create.Required, want) {
			t.Errorf("required = %v, want it to name %q — a spec that omits a field "+
				"the server demands describes a request that always fails", create.Required, want)
		}
	}
}

// No field is dropped, whatever its type. One whose shape cannot be inferred is
// published without a type constraint, which is true rather than absent.
func TestOpenAPISchemaOmitsNoField(t *testing.T) {
	spec, name := scSpec(t, scModel{})
	props := scProps(t, spec, name)
	for _, want := range []string{"name", "tags", "needed", "seal", "price", "fee", "occurred"} {
		if _, ok := props[want]; !ok {
			t.Errorf("field %q is missing from the generated schema", want)
		}
	}
}

// …and the model says so at boot, because an unconstrained schema is a fallback,
// not an answer.
func TestUninferableFieldIsAStrictStartupError(t *testing.T) {
	s := New(Config{DisableAutoMigrate: true, Strict: true})
	if err := s.Register(scModel{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	err := s.validateRegistry()
	if err == nil {
		t.Fatal("a field with no inferable schema passed strict validation")
	}
	for _, want := range []string{"scModel", "price", "fee", "ObjectWithSchema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	// The fields the generator can describe must not be reported.
	for _, quiet := range []string{"tags", "seal", "occurred", "name"} {
		if strings.Contains(err.Error(), `"`+quiet+`"`) {
			t.Errorf("%q was reported, but its schema is inferable: %v", quiet, err)
		}
	}
}

// Not strict is not silent: the cost lands on whoever generates a client, who is
// not the person reading startup errors.
func TestUninferableFieldWarnsWhenNotStrict(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	s := New(Config{DisableAutoMigrate: true, Logger: logger})
	if err := s.Register(scModel{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.validateRegistry(); err != nil {
		t.Fatalf("a non-strict server must still start: %v", err)
	}
	warnUninferableSchemas(s.Registry(), &s.cfg, logger)

	if !strings.Contains(buf.String(), "price") {
		t.Errorf("boot logged no warning naming the field:\n%s", buf.String())
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
