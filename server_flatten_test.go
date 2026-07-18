package maniflex

import "testing"

type flatA struct{ BaseModel }
type flatB struct{ BaseModel }

// modelTableNames returns the registration-order list of model Go type names so
// tests can assert which structs flattenArgs treated as models (and, by
// exclusion, that ModelConfig values were NOT mistaken for models).
func modelTypeNames(models []any) []string {
	names := make([]string, len(models))
	for i, m := range models {
		switch m.(type) {
		case flatA:
			names[i] = "flatA"
		case flatB:
			names[i] = "flatB"
		default:
			names[i] = "?"
		}
	}
	return names
}

// A ModelConfig inlined inside a slice must bind to the model element that
// precedes it, not be flattened in as a bogus model. Regression for the panic
// "model must embed BaseModel" when using the MustRegister(domain.Models()...)
// layout where each Models() slice carries its own inline ModelConfigs.
func TestFlattenArgs_ModelConfigInsideSlice(t *testing.T) {
	models, configs, err := flattenArgs([]any{
		[]any{flatA{}, ModelConfig{TableName: "a_tbl"}},
		[]any{flatB{}, ModelConfig{TableName: "b_tbl"}},
	})
	if err != nil {
		t.Fatalf("flattenArgs: unexpected error: %v", err)
	}

	if got := modelTypeNames(models); len(got) != 2 || got[0] != "flatA" || got[1] != "flatB" {
		t.Fatalf("expected models [flatA flatB], got %v", got)
	}
	if configs[0].TableName != "a_tbl" {
		t.Fatalf("config for flatA should be a_tbl, got %q", configs[0].TableName)
	}
	if configs[1].TableName != "b_tbl" {
		t.Fatalf("config for flatB should be b_tbl, got %q", configs[1].TableName)
	}
}

// A ModelConfig at the start of a slice binds to a model passed as a preceding
// top-level argument (the pairing rule crosses the slice boundary).
func TestFlattenArgs_ModelConfigFirstInSliceBindsToPriorModel(t *testing.T) {
	models, configs, err := flattenArgs([]any{
		flatA{},
		[]any{ModelConfig{TableName: "a_tbl"}},
	})
	if err != nil {
		t.Fatalf("flattenArgs: unexpected error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d (%v)", len(models), modelTypeNames(models))
	}
	if configs[0].TableName != "a_tbl" {
		t.Fatalf("config should bind to flatA, got %q", configs[0].TableName)
	}
}

// Top-level pairing (the pre-existing behaviour) must keep working.
func TestFlattenArgs_TopLevelPairingUnchanged(t *testing.T) {
	models, configs, err := flattenArgs([]any{
		flatA{}, ModelConfig{TableName: "a_tbl"},
		flatB{}, ModelConfig{TableName: "b_tbl"},
	})
	if err != nil {
		t.Fatalf("flattenArgs: unexpected error: %v", err)
	}
	if got := modelTypeNames(models); len(got) != 2 || got[0] != "flatA" || got[1] != "flatB" {
		t.Fatalf("expected models [flatA flatB], got %v", got)
	}
	if configs[0].TableName != "a_tbl" || configs[1].TableName != "b_tbl" {
		t.Fatalf("unexpected configs: %q, %q", configs[0].TableName, configs[1].TableName)
	}
}
