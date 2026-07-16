package maniflex_test

// MS-1 — mfx:"hidden" means the client may neither read nor write the field;
// that is exactly what distinguishes it from mfx:"writeonly" ("the client writes
// it and never reads it back" — a password). Only the read half was enforced,
// so a bare hidden field was silently accepted from a request body while the
// docs and the generated OpenAPI write schemas both said it could not be. Hidden
// now implies Readonly, unless writeonly explicitly says otherwise.

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

type hiddenTagModel struct {
	maniflex.BaseModel
	Name string `json:"name"`

	// The scenario from the audit: invisible in responses, and — until this fix —
	// settable by anyone willing to guess the column name.
	IsAdmin bool `json:"is_admin" mfx:"hidden"`

	// writeonly is the deliberate "client writes it, never reads it" case.
	Password string `json:"password" mfx:"writeonly"`

	// Contradictory pair. Nothing in the framework uses it; the explicit
	// directive wins, so it stays writable and today's behaviour is preserved.
	HiddenWriteOnly string `json:"hidden_writeonly" mfx:"hidden,writeonly"`

	// Tag order must not change the outcome.
	WriteOnlyHidden string `json:"writeonly_hidden" mfx:"writeonly,hidden"`

	// Spelling out both is still allowed and must not be disturbed.
	Explicit string `json:"explicit" mfx:"hidden,readonly"`

	// A plain field must not acquire either flag by accident.
	Plain string `json:"plain"`
}

func TestHiddenTag_ImpliesReadonly(t *testing.T) {
	meta := scan(t, hiddenTagModel{})

	f := findField(meta, "IsAdmin")
	if f == nil {
		t.Fatal("hidden field must still be a column")
	}
	if !f.Tags.Hidden {
		t.Error(`mfx:"hidden" should set Hidden`)
	}
	if !f.Tags.Readonly {
		t.Error(`mfx:"hidden" must imply Readonly — otherwise the field is invisible in ` +
			`responses but still settable from a request body, which is the mass-assignment ` +
			`hole the "hidden" name promises to close`)
	}
}

// writeonly must stay writable — it is the whole point of the directive, and a
// password field that silently stopped being accepted would be a bad way to
// find out.
func TestWriteOnlyTag_StaysWritable(t *testing.T) {
	meta := scan(t, hiddenTagModel{})

	f := findField(meta, "Password")
	if f == nil {
		t.Fatal("writeonly field must still be a column")
	}
	if !f.Tags.WriteOnly {
		t.Error(`mfx:"writeonly" should set WriteOnly`)
	}
	if f.Tags.Readonly {
		t.Error(`mfx:"writeonly" must not be Readonly — the client is supposed to write it`)
	}
	if f.Tags.Hidden {
		t.Error(`mfx:"writeonly" should not set Hidden — it is excluded from responses by ` +
			`WriteOnly, and setting Hidden too would drag in the readonly implication`)
	}
}

// An explicit writeonly outranks hidden's implication, whichever order they
// appear in.
func TestHiddenWithWriteOnly_StaysWritable(t *testing.T) {
	meta := scan(t, hiddenTagModel{})

	for _, name := range []string{"HiddenWriteOnly", "WriteOnlyHidden"} {
		f := findField(meta, name)
		if f == nil {
			t.Fatalf("%s must still be a column", name)
		}
		if !f.Tags.Hidden || !f.Tags.WriteOnly {
			t.Errorf("%s: got Hidden=%v WriteOnly=%v, want both true",
				name, f.Tags.Hidden, f.Tags.WriteOnly)
		}
		if f.Tags.Readonly {
			t.Errorf("%s: hidden's Readonly implication overrode an explicit writeonly — "+
				"the explicit directive should win", name)
		}
	}
}

func TestHiddenTag_ExplicitReadonlyUnaffected(t *testing.T) {
	meta := scan(t, hiddenTagModel{})

	f := findField(meta, "Explicit")
	if f == nil {
		t.Fatal("hidden,readonly field must still be a column")
	}
	if !f.Tags.Hidden || !f.Tags.Readonly {
		t.Errorf(`mfx:"hidden,readonly": got Hidden=%v Readonly=%v, want both true`,
			f.Tags.Hidden, f.Tags.Readonly)
	}
}

// The implication must not leak onto fields that asked for neither.
func TestHiddenTag_PlainFieldUntouched(t *testing.T) {
	meta := scan(t, hiddenTagModel{})

	f := findField(meta, "Plain")
	if f == nil {
		t.Fatal("plain field must be a column")
	}
	if f.Tags.Hidden || f.Tags.Readonly || f.Tags.WriteOnly {
		t.Errorf("an untagged field got Hidden=%v Readonly=%v WriteOnly=%v, want all false",
			f.Tags.Hidden, f.Tags.Readonly, f.Tags.WriteOnly)
	}
}

// ── hidden + required is unsatisfiable ────────────────────────────────────────

type hiddenRequiredModel struct {
	maniflex.BaseModel
	Token string `json:"token" mfx:"hidden,required"`
}

// hidden now strips the field from writes, so a required check on it can never
// pass: the NOT NULL column rejects every insert with "<field> is required",
// including requests that did supply it. Registration must refuse the pair
// rather than let that ship — the runtime message points the caller at a
// mistake they are not making.
func TestHiddenRequired_RejectedAtRegistration(t *testing.T) {
	_, err := maniflex.ScanModel(hiddenRequiredModel{}, maniflex.ModelConfig{})
	if err == nil {
		t.Fatal(`mfx:"hidden,required" registered successfully — every create against this ` +
			`model fails at runtime with "token is required" even when token is sent`)
	}
	for _, want := range []string{"hidden", "required", "Token", `writeonly`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so it does not say how to fix it: %v", want, err)
		}
	}
}

type writeOnlyRequiredModel struct {
	maniflex.BaseModel
	Password string `json:"password" mfx:"writeonly,required"`
}

// The combination the error tells you to use must actually work — otherwise the
// rejection sends people somewhere just as broken.
func TestWriteOnlyRequired_Accepted(t *testing.T) {
	meta, err := maniflex.ScanModel(writeOnlyRequiredModel{}, maniflex.ModelConfig{})
	if err != nil {
		t.Fatalf(`mfx:"writeonly,required" must register — it is what the hidden+required `+
			`error tells callers to switch to: %v`, err)
	}
	f := findField(meta, "Password")
	if f == nil {
		t.Fatal("writeonly,required field must be a column")
	}
	if !f.Tags.Required || !f.Tags.WriteOnly || f.Tags.Readonly {
		t.Errorf("got Required=%v WriteOnly=%v Readonly=%v, want required+writeonly and not readonly",
			f.Tags.Required, f.Tags.WriteOnly, f.Tags.Readonly)
	}
}

type hiddenWriteOnlyRequiredModel struct {
	maniflex.BaseModel
	Token string `json:"token" mfx:"hidden,writeonly,required"`
}

// hidden+writeonly+required is satisfiable: writeonly keeps the field writable,
// so required can be met. Only the pair without writeonly is contradictory.
func TestHiddenWriteOnlyRequired_Accepted(t *testing.T) {
	meta, err := maniflex.ScanModel(hiddenWriteOnlyRequiredModel{}, maniflex.ModelConfig{})
	if err != nil {
		t.Fatalf(`mfx:"hidden,writeonly,required" is satisfiable (writeonly keeps it `+
			`writable) and must register: %v`, err)
	}
	if f := findField(meta, "Token"); f == nil || f.Tags.Readonly {
		t.Errorf("Token: got %+v, want a writable column", f)
	}
}
