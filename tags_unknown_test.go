package maniflex

// MS-2 — an unrecognised mfx: option used to be discarded in silence. For a
// descriptive directive that is merely annoying; for a protective one it is a
// security hole, because `mfx:"read_only"` leaves Readonly false and the field
// client-writable with nothing anywhere saying so. Unknown options are now a
// registration error that names the option it thinks you meant.

import (
	"reflect"
	"strings"
	"testing"
)

// tagsFor parses an mfx tag literal the way a real struct field would be parsed.
func tagsFor(t *testing.T, mfx string) FieldTags {
	t.Helper()
	return parseFieldTags(reflect.StructField{
		Name: "Field", Type: reflect.TypeOf(""),
		Tag: reflect.StructTag(`mfx:"` + mfx + `"`),
	})
}

// The trap: a field with no mfx tag splits to [""], and so does a trailing
// comma. Treating an empty part as unknown would reject every untagged field in
// the world, which is most of them.
func TestUnknownOpts_EmptyPartsAreNotUnknown(t *testing.T) {
	for _, mfx := range []string{"", "required,", ",required", "required,,readonly", ","} {
		if got := tagsFor(t, mfx); len(got.UnknownOpts) > 0 {
			t.Errorf("mfx:%q reported unknown options %q — an empty comma-part is not a "+
				"typo (a field with no mfx tag at all parses to one empty part)", mfx, got.UnknownOpts)
		}
	}
}

// A field with no mfx tag must be entirely untouched.
func TestUnknownOpts_NoTag(t *testing.T) {
	f := parseFieldTags(reflect.StructField{
		Name: "Plain", Type: reflect.TypeOf(""), Tag: reflect.StructTag(`json:"plain"`),
	})
	if len(f.UnknownOpts) > 0 {
		t.Errorf("a field with no mfx tag reported unknown options %q", f.UnknownOpts)
	}
}

func TestUnknownOpts_RecognisedOptionsAreNotFlagged(t *testing.T) {
	// One representative of every shape the switch handles.
	for _, mfx := range []string{
		"required", "readonly", "immutable", "filterable", "sortable", "hidden",
		"writeonly", "unique", "index", "relation", "norelation", "searchable",
		"encrypted", "file", "locale", "scheduled", "auto_delete:false",
		"split", "resolve", "dynamic",
		"enum:a|b", "min:1", "max:10", "default:x", "key:k", "max_size:2MB",
		"accept:image/*", "lock_when:status=paid", "lock_scope:Stock",
		"cursor_field:created_at", "file_acl:signed", "default_locale:en",
		"relation:Manager", "relation:Manager;onDelete:cascade", "through:Junction",
		"scheduled;soft_delete",
		"required,readonly,filterable", // multiple
		"required, readonly",           // whitespace after the comma
	} {
		if got := tagsFor(t, mfx); len(got.UnknownOpts) > 0 {
			t.Errorf("mfx:%q flagged %q as unknown — a valid option is being rejected",
				mfx, got.UnknownOpts)
		}
	}
}

func TestUnknownOpts_Collected(t *testing.T) {
	got := tagsFor(t, "required,read_only,frobnicate")
	want := []string{"read_only", "frobnicate"}
	if len(got.UnknownOpts) != len(want) {
		t.Fatalf("UnknownOpts = %q, want %q", got.UnknownOpts, want)
	}
	for i := range want {
		if got.UnknownOpts[i] != want[i] {
			t.Errorf("UnknownOpts[%d] = %q, want %q", i, got.UnknownOpts[i], want[i])
		}
	}
	// The recognised option alongside them must still have been applied.
	if !got.Required {
		t.Error("a valid option next to an unknown one was not applied")
	}
}

// The three shapes the audit calls out, each of which silently left a field
// writable.
func TestSuggestOpt_AuditTypos(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"read_only", "readonly"}, // a stray separator
		{"Readonly", "readonly"},  // wrong case
		{"reaodnly", "readonly"},  // transposition
	} {
		if got := suggestOpt(tc.in); got != tc.want {
			t.Errorf("suggestOpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSuggestOpt_WrongCase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"REQUIRED", "required"},
		{"Hidden", "hidden"},
		{"WriteOnly", "writeonly"},
		{"Enum:a|b", "enum:"},
		{"Min:3", "min:"},
	} {
		if got := suggestOpt(tc.in); got != tc.want {
			t.Errorf("suggestOpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A value-carrying option written without its value.
func TestSuggestOpt_MissingValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"lock_scope", "lock_scope:"},
		{"enum", "enum:"},
		{"cursor_field", "cursor_field:"},
	} {
		if got := suggestOpt(tc.in); got != tc.want {
			t.Errorf("suggestOpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A guess that is wrong is worse than no guess: it sends the reader off to
// change something that was not the problem.
func TestSuggestOpt_NoWildGuesses(t *testing.T) {
	for _, in := range []string{"frobnicate", "completely_made_up", "xyzzy"} {
		if got := suggestOpt(in); got != "" {
			t.Errorf("suggestOpt(%q) = %q, want no suggestion — guessing at an option "+
				"that shares nothing with the input misdirects the reader", in, got)
		}
	}
	// "min" and "max" are one edit apart. Suggesting one for the other would be
	// actively harmful: both are real options with opposite meanings.
	if got := suggestOpt("mix:3"); got == "min:" || got == "max:" {
		t.Errorf("suggestOpt(%q) = %q — guessing between two real options that mean "+
			"opposite things is worse than saying nothing", "mix:3", got)
	}
}

func TestUnknownOptError_MessageNamesFieldAndSuggestion(t *testing.T) {
	err := unknownOptError("User", "Role", []string{"read_only"})
	for _, want := range []string{"User", "Role", "read_only", "readonly", "did you mean"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message lacks %q: %v", want, err)
		}
	}
}

// An option with nothing close to it still has to be reported — just without a
// misleading guess attached.
func TestUnknownOptError_NoSuggestionAvailable(t *testing.T) {
	err := unknownOptError("User", "Role", []string{"frobnicate"})
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error does not name the unknown option: %v", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("error offers a suggestion it does not have: %v", err)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"readonly", "readonly", 0},
		{"readonly", "read_only", 1}, // insert
		{"readonly", "readonl", 1},   // delete
		{"readonly", "reaodnly", 2},  // transposition = two edits
		{"", "abc", 3},
		{"abc", "", 3},
	} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ── Registration ──────────────────────────────────────────────────────────────
// The parser only records; ScanModel is what refuses. These are the tests that
// pin the actual security property.

type typoModel struct {
	BaseModel
	Role string `json:"role" mfx:"read_only"` // meant readonly — silently left writable
}

func TestScanModel_RejectsUnknownOption(t *testing.T) {
	_, err := ScanModel(typoModel{}, ModelConfig{})
	if err == nil {
		t.Fatal(`a model with mfx:"read_only" registered successfully — the directive is ` +
			`not applied, so Role stays client-writable and PATCH {"role":"admin"} lands`)
	}
	for _, want := range []string{"typoModel", "Role", "read_only", "readonly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("registration error lacks %q: %v", want, err)
		}
	}
}

type typoSliceModel struct {
	BaseModel
	Name  string       `json:"name"`
	Items []typoDetail `json:"items" mfx:"filterabel"` // HasMany — becomes a Relation
}
type typoDetail struct {
	BaseModel
	Label string `json:"label"`
}

// HasMany and many-to-many slices become Relations and never land in
// meta.Fields, so a check that walked meta.Fields would miss them. The check
// runs per field as it is collected, which covers every bucket.
func TestScanModel_RejectsUnknownOptionOnSliceField(t *testing.T) {
	_, err := ScanModel(typoSliceModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("a typo'd option on a HasMany slice field registered successfully — the " +
			"check is not covering fields that become Relations rather than Fields")
	}
	if !strings.Contains(err.Error(), "filterabel") {
		t.Errorf("error does not name the unknown option: %v", err)
	}
}

type validTagsModel struct {
	BaseModel
	Name  string `json:"name"  mfx:"required,filterable,sortable"`
	Email string `json:"email" mfx:"unique,index"`
	Score int    `json:"score" mfx:"min:0,max:100"`
	Plain string `json:"plain"`
}

func TestScanModel_AcceptsValidTags(t *testing.T) {
	if _, err := ScanModel(validTagsModel{}, ModelConfig{}); err != nil {
		t.Fatalf("a model with only valid tags failed to register: %v", err)
	}
}

// BaseModel carries mfx:"versioned" / "cursor_field:", which ScanModel reads
// itself and the field parser has no case for. collectFields recurses into an
// anonymous embed without parsing its tag, so these never reach the check — but
// that is a load-bearing accident worth pinning, since the whole documented
// versioning feature rests on it.
type versionedEmbedModel struct {
	BaseModel `mfx:"versioned"`
	Name      string `json:"name"`
}
type diffOnlyEmbedModel struct {
	BaseModel `mfx:"versioned:diff_only"`
	Name      string `json:"name"`
}
type cursorEmbedModel struct {
	BaseModel `mfx:"cursor_field:created_at"`
	Name      string `json:"name"`
}

func TestScanModel_BaseModelEmbedTagsSurvive(t *testing.T) {
	m1, err := ScanModel(versionedEmbedModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf(`mfx:"versioned" on the BaseModel embed must still register — the unknown-`+
			`option check is reaching the embed, which the field parser has no case for: %v`, err)
	}
	if !m1.Config.Versioned {
		t.Error(`mfx:"versioned" did not enable versioning`)
	}

	m2, err := ScanModel(diffOnlyEmbedModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf(`mfx:"versioned:diff_only" on the embed must still register: %v`, err)
	}
	if !m2.Config.Versioned || !m2.Config.VersionedDiffOnly {
		t.Error(`mfx:"versioned:diff_only" did not enable diff-only versioning`)
	}

	m3, err := ScanModel(cursorEmbedModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf(`mfx:"cursor_field:" on the embed must still register: %v`, err)
	}
	if m3.CursorField != "created_at" {
		t.Errorf("CursorField = %q, want %q", m3.CursorField, "created_at")
	}
}

// knownBareOpts/knownPrefixOpts drive the suggestions, and they are a hand-kept
// copy of the parser's switch. If the two drift, a real option starts being
// reported as unknown — so every option the lists claim must actually parse.
func TestKnownOptLists_MatchTheParser(t *testing.T) {
	for _, opt := range knownBareOpts {
		if got := tagsFor(t, opt); len(got.UnknownOpts) > 0 {
			t.Errorf("knownBareOpts lists %q but the parser does not recognise it — the "+
				"suggestion lists have drifted from the switch", opt)
		}
	}
	for _, opt := range knownPrefixOpts {
		// Give each prefix a plausible value; the point is the key is recognised.
		probe := opt + "x"
		if opt == "scheduled;" {
			probe = "scheduled;soft_delete"
		}
		if got := tagsFor(t, probe); len(got.UnknownOpts) > 0 {
			t.Errorf("knownPrefixOpts lists %q but the parser does not recognise %q — the "+
				"suggestion lists have drifted from the switch", opt, probe)
		}
	}
}
