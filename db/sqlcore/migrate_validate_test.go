package sqlcore

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// validateMappableColumns must let scalar, time.Time, and SQLTyper-backed
// columns through while rejecting bare map / slice fields that would otherwise
// be silently dropped during migration.
func TestValidateMappableColumns(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}

	good := &maniflex.ModelMeta{
		Name: "Good",
		Fields: []maniflex.FieldMeta{
			{Name: "Title", Type: reflect.TypeOf("")},
			{Name: "Count", Type: reflect.TypeOf(0)},
			{Name: "Label", Type: reflect.TypeOf(maniflex.LocaleString{})}, // SQLTyper
		},
	}
	if err := a.validateMappableColumns(good); err != nil {
		t.Fatalf("mappable model should pass, got: %v", err)
	}

	t.Run("rejects bare map", func(t *testing.T) {
		bad := &maniflex.ModelMeta{
			Name: "Bad",
			Fields: []maniflex.FieldMeta{
				{Name: "Title", Type: reflect.TypeOf("")},
				{Name: "Meta", Type: reflect.TypeOf(map[string]any{})},
			},
		}
		err := a.validateMappableColumns(bad)
		if err == nil {
			t.Fatal("unmappable map field should be rejected, got nil")
		}
		// Error must name the offending field and point at the remedy.
		if !strings.Contains(err.Error(), "Meta") || !strings.Contains(err.Error(), "SQLTyper") {
			t.Fatalf("error should name the field and the SQLTyper remedy, got: %v", err)
		}
	})

	t.Run("rejects bare slice", func(t *testing.T) {
		bad := &maniflex.ModelMeta{
			Name: "BadSlice",
			Fields: []maniflex.FieldMeta{
				{Name: "Tags", Type: reflect.TypeOf([]string{})},
			},
		}
		if err := a.validateMappableColumns(bad); err == nil {
			t.Fatal("unmappable slice field should be rejected, got nil")
		}
	})
}
