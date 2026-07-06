package maniflex_test

// Coverage for the json:"-" semantics change: json:"-" now hides a field from
// API responses (Hidden) and marks it server-owned (Readonly) while keeping it as
// a real column. db:"-" and mfx:"-" remain the true "not a column" opt-outs.

import (
	"testing"

	"github.com/xaleel/maniflex"
)

type jsonHideModel struct {
	maniflex.BaseModel
	Name        string `json:"name"`
	Token       string `json:"-"`                    // hidden + readonly, still persisted
	DBExcluded  string `json:"db_excluded" db:"-"`   // not a column
	MfxExcluded string `json:"mfx_excluded" mfx:"-"` // not a column
}

func findField(meta *maniflex.ModelMeta, name string) *maniflex.FieldMeta {
	for i := range meta.Fields {
		if meta.Fields[i].Name == name {
			return &meta.Fields[i]
		}
	}
	return nil
}

func TestJSONDash_HidesButPersists(t *testing.T) {
	meta := scan(t, jsonHideModel{})

	tok := findField(meta, "Token")
	if tok == nil {
		t.Fatal(`json:"-" field must still be a column (found no Token field)`)
	}
	if !tok.Tags.Hidden {
		t.Error(`json:"-" field should be Hidden (excluded from responses)`)
	}
	if !tok.Tags.Readonly {
		t.Error(`json:"-" field should be Readonly (server-owned, clients can't write it)`)
	}
	if tok.Tags.Ignore {
		t.Error(`json:"-" field must not be Ignored (that would drop the column)`)
	}
	if tok.Tags.DBName != "token" {
		t.Errorf(`json:"-" field DBName: got %q, want "token"`, tok.Tags.DBName)
	}
}

func TestDBDash_And_MfxDash_DropColumn(t *testing.T) {
	meta := scan(t, jsonHideModel{})
	if f := findField(meta, "DBExcluded"); f != nil {
		t.Error(`db:"-" field must not be a column`)
	}
	if f := findField(meta, "MfxExcluded"); f != nil {
		t.Error(`mfx:"-" field must not be a column`)
	}
}
