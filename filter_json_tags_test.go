package maniflex

// mfx:"json_array" / mfx:"json_object" say what a column holds, and the `has`
// operator is compiled from that answer. A tag that disagrees with the field's
// own type produces SQL asking a JSON question of something that is not JSON —
// a filter that returns nothing, or an error from the driver, depending on which
// one is running. Caught at registration, where the mistake actually is.

import (
	"strings"
	"testing"
	"time"
)

type jtArrayOnString struct {
	BaseModel
	Tags string `json:"tags" mfx:"filterable,json_array"`
}

type jtObjectOnInt struct {
	BaseModel
	Meta int `json:"meta" mfx:"filterable,json_object"`
}

type jtBothTags struct {
	BaseModel
	Blob map[string]any `json:"blob" mfx:"filterable,json_array,json_object"`
}

type jtArrayOnBytes struct {
	BaseModel
	Blob []byte `json:"blob" mfx:"filterable,json_array"`
}

type jtObjectOnTime struct {
	BaseModel
	When time.Time `json:"when" mfx:"filterable,json_object"`
}

type jtValid struct {
	BaseModel
	Tags   []string       `json:"tags"   mfx:"filterable,json_array"`
	Meta   map[string]any `json:"meta"   mfx:"filterable,json_object"`
	Locale LocaleString   `json:"locale" mfx:"locale"`
}

func registerJSONTagged(t *testing.T, model any) error {
	t.Helper()
	return New(Config{DisableAutoMigrate: true}).Register(model)
}

func TestJSONColumnTagsAreCheckedAgainstTheFieldType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		want  []string
	}{
		{"json_array on a string field", jtArrayOnString{}, []string{"tags", "json_array"}},
		{"json_object on an int field", jtObjectOnInt{}, []string{"meta", "json_object"}},
		{"json_object on a time.Time field", jtObjectOnTime{}, []string{"when", "json_object"}},
		// A byte slice is a blob, not an array of elements: json_each over it
		// would walk bytes, which is not what anyone tagging it meant.
		{"json_array on a byte slice", jtArrayOnBytes{}, []string{"blob", "json_array"}},
		{"both tags on one field", jtBothTags{}, []string{"blob", "json_array", "json_object"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := registerJSONTagged(t, tc.model)
			if err == nil {
				t.Fatal("registered")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
		})
	}

	t.Run("a correctly tagged model registers", func(t *testing.T) {
		if err := registerJSONTagged(t, jtValid{}); err != nil {
			t.Fatalf("rejected a valid model: %v", err)
		}
	})
}
