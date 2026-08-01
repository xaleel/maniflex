package maniflex

import (
	"strings"
	"testing"
)

type baseTagsModel struct {
	BaseModel
	Name string `json:"name" db:"name"`
}

func scanWithBaseTags(t *testing.T, tags map[string]string) (*ModelMeta, error) {
	t.Helper()
	return ScanModel(baseTagsModel{}, ModelConfig{BaseModelTags: tags})
}

func TestBaseModelTags_EachAllowedOptionApplies(t *testing.T) {
	m, err := scanWithBaseTags(t, map[string]string{
		"id":         "filterable,sortable",
		"created_at": "filterable,sortable,index,hidden",
		"updated_at": "filterable,sortable,index,hidden",
	})
	if err != nil {
		t.Fatalf("every allowed option must register: %v", err)
	}
	for _, col := range []string{"id", "created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if f == nil {
			t.Fatalf("column %q missing from the scanned model", col)
		}
		if !f.Tags.Filterable || !f.Tags.Sortable {
			t.Errorf("%s: filterable=%v sortable=%v, want both true",
				col, f.Tags.Filterable, f.Tags.Sortable)
		}
	}
	for _, col := range []string{"created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if !f.Tags.Index || !f.Tags.Hidden {
			t.Errorf("%s: index=%v hidden=%v, want both true",
				col, f.Tags.Index, f.Tags.Hidden)
		}
	}
}

// The union must never cost the default. A replace-style implementation would
// leave created_at client-writable here, which is the whole reason this is a
// union and not an assignment.
func TestBaseModelTags_ReadonlySurvivesTheUnion(t *testing.T) {
	m, err := scanWithBaseTags(t, map[string]string{
		"id":         "filterable",
		"created_at": "filterable",
		"updated_at": "filterable",
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, col := range readonlyBaseColumns {
		if f := m.FieldByDBName(col); !f.Tags.Readonly {
			t.Errorf(`%s lost mfx:"readonly" — BaseModelTags must only widen`, col)
		}
	}
}

// readonlyBaseColumns are the BaseModel columns carrying mfx:"readonly" — all
// of them. Kept as a named list so this and TestBaseModel_DefaultsAreReadonlyOnly
// cannot disagree about which columns are framework-managed.
var readonlyBaseColumns = []string{"id", "created_at", "updated_at"}

func TestBaseModelTags_UnknownKeyRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"createdat": "filterable"})
	if err == nil {
		t.Fatal("an unknown BaseModelTags key must be a registration error")
	}
	if !strings.Contains(err.Error(), `did you mean "created_at"?`) {
		t.Errorf("error should suggest the intended column, got: %v", err)
	}
}

func TestBaseModelTags_DisallowedOptionRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"id": "file"})
	if err == nil {
		t.Fatal(`mfx:"file" on the id column must be a registration error`)
	}
	if !strings.Contains(err.Error(), "allowed: filterable, sortable") {
		t.Errorf("error should list what the id column does accept, got: %v", err)
	}
}

// id is the primary key and already indexed; buildIndices would emit a
// redundant idx_<table>_id with real write cost.
func TestBaseModelTags_IndexOnIDRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"id": "index"})
	if err == nil {
		t.Fatal("index on the id column must be rejected — id is the primary key")
	}
}

func TestBaseModelTags_TypoSuggestsAllowedOption(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"created_at": "filterble"})
	if err == nil {
		t.Fatal("a misspelt option must be a registration error")
	}
	if !strings.Contains(err.Error(), `did you mean "filterable"?`) {
		t.Errorf("error should suggest the intended option, got: %v", err)
	}
}

// Map iteration is randomised; the error must not be. Without sorted keys a
// config with two bad entries reports a different error on each run, so fixing
// the reported one surfaces the other.
func TestBaseModelTags_ErrorIsDeterministic(t *testing.T) {
	tags := map[string]string{"created_at": "bogus", "updated_at": "alsobogus"}
	var first string
	for i := range 50 {
		_, err := scanWithBaseTags(t, tags)
		if err == nil {
			t.Fatal("two invalid options must be a registration error")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("error varies between runs — keys must be sorted before "+
				"iterating:\n  %s\n  %s", first, err.Error())
		}
	}
}

// Same trap as TestUnknownOpts_EmptyPartsAreNotUnknown: "" and a trailing comma
// both split to one empty part, and neither is a typo.
func TestBaseModelTags_EmptyPartsTolerated(t *testing.T) {
	for _, spec := range []string{"", "filterable,", ",filterable", "filterable,,sortable", ","} {
		if _, err := scanWithBaseTags(t, map[string]string{"id": spec}); err != nil {
			t.Errorf("BaseModelTags[\"id\"]=%q must register — an empty comma-part "+
				"is not a typo: %v", spec, err)
		}
	}
}

// The defaults are the whole point of the change: filterable and sortable widen
// a model's public query surface, so they are opt-in per model rather than
// inherited by every model that embeds BaseModel. This test is what fails if a
// default is quietly reintroduced later.
func TestBaseModel_DefaultsAreReadonlyOnly(t *testing.T) {
	m, err := ScanModel(baseTagsModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, col := range []string{"id", "created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if f == nil {
			t.Fatalf("column %q missing from the scanned model", col)
		}
		if !f.Tags.Readonly {
			t.Errorf("%s: readonly=false, want true — BaseModel columns are "+
				"framework-managed", col)
		}
		if f.Tags.Filterable || f.Tags.Sortable || f.Tags.Index || f.Tags.Hidden {
			t.Errorf("%s: filterable=%v sortable=%v index=%v hidden=%v — all must "+
				"be false by default; opt in via ModelConfig.BaseModelTags",
				col, f.Tags.Filterable, f.Tags.Sortable, f.Tags.Index, f.Tags.Hidden)
		}
	}
}

// cursor_field does not implicitly grant sortable. The missing half is a loud
// registration error rather than a silent widening, so the model's query
// surface stays exactly what its config says.
func TestBaseModelTags_CursorFieldStillNeedsSortable(t *testing.T) {
	_, err := ScanModel(cursorEmbedModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("cursor_field:created_at without BaseModelTags must fail to register")
	}
	if !strings.Contains(err.Error(), "sortable") {
		t.Errorf("error should name the missing option, got: %v", err)
	}
	if !strings.Contains(err.Error(), "BaseModelTags") {
		t.Errorf("error should name the knob that fixes it, got: %v", err)
	}

	if _, err := ScanModel(cursorEmbedModel{}, cursorEmbedConfig); err != nil {
		t.Fatalf("cursor_field:created_at with the matching BaseModelTags must "+
			"register: %v", err)
	}
}

// The remedy has to name a knob the reader can actually turn. BaseModel is
// framework-owned, so `add mfx:"sortable" to the struct tag` points at a struct
// in the framework's own source tree.
func TestHowToAllow_BaseModelColumnNamesTheConfig(t *testing.T) {
	got := howToAllow("created_at", "sortable")
	if !strings.Contains(got, "ModelConfig.BaseModelTags") {
		t.Errorf("howToAllow(created_at) = %q, want it to name ModelConfig.BaseModelTags", got)
	}
	if !strings.Contains(got, `"created_at"`) {
		t.Errorf("howToAllow(created_at) = %q, want it to name the column", got)
	}
}

func TestHowToAllow_OrdinaryColumnNamesTheStructTag(t *testing.T) {
	got := howToAllow("title", "filterable")
	if !strings.Contains(got, `mfx:"filterable"`) {
		t.Errorf("howToAllow(title) = %q, want it to name the struct tag", got)
	}
	if strings.Contains(got, "BaseModelTags") {
		t.Errorf("howToAllow(title) = %q, must not mention BaseModelTags for an "+
			"ordinary column", got)
	}
}

// baseModelTagOptions is a hand-kept list of option spellings. If one drifts
// from the parser, BaseModelTags starts rejecting an option that exists — the
// same failure TestKnownOptLists_MatchTheParser guards for the suggestion lists.
func TestBaseModelTagOptions_MatchTheParser(t *testing.T) {
	for col, opts := range baseModelTagOptions {
		for _, opt := range opts {
			if got := tagsFor(t, opt); len(got.UnknownOpts) > 0 {
				t.Errorf("baseModelTagOptions[%q] names %q, which parseFieldTags "+
					"does not recognise", col, opt)
			}
		}
	}
}
