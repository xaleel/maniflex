package maniflex

// R5 — registration guards for mfx:"upload:presigned".
//
// The tag is opt-in and protective-adjacent: it changes how a file field's bytes
// reach storage, and the field's max_size/accept rules are what bound the
// authorisation it mints. Both ways of writing it wrong therefore have to be
// registration errors rather than quiet no-ops — the same rule v0.2.3 applied to
// every other unrecognised option.

import (
	"strings"
	"testing"
)

type presignNoFile struct {
	BaseModel
	Note string `json:"note" db:"note" mfx:"upload:presigned"`
}

// upload:presigned on a field that stores no file mounts no route and bounds
// nothing. It parses, so the unknown-option check cannot catch it.
func TestPresignedUpload_WithoutFileIsRejected(t *testing.T) {
	_, err := ScanModel(presignNoFile{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted upload:presigned on a non-file field — a directive that " +
			"does nothing, in silence")
	}
	if !strings.Contains(err.Error(), "upload:presigned") || !strings.Contains(err.Error(), "file") {
		t.Errorf("error does not name the directive and the fix: %v", err)
	}
}

type presignBadValue struct {
	BaseModel
	Doc string `json:"doc" db:"doc" mfx:"file,upload:presined"`
}

// A misspelt value must be an unknown-option error. Silently ignoring it would
// leave the field on the multipart path while its author believed otherwise.
func TestPresignedUpload_MisspeltValueIsRejected(t *testing.T) {
	_, err := ScanModel(presignBadValue{}, ModelConfig{})
	if err == nil {
		t.Fatal(`ScanModel accepted mfx:"upload:presined"`)
	}
	if !strings.Contains(err.Error(), "upload:presined") {
		t.Errorf("error does not name the bad option: %v", err)
	}
}

// A value-constrained option is known by its whole spelling, so it lives in
// knownBareOpts — and suggestOpt routes anything with a ":" at the prefix list
// alone, which used to mean no misspelling of one ever got a suggestion. That was
// already true of auto_delete:false before upload:presigned joined it.
func TestSuggestOpt_ValueConstrainedOptions(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"upload:presined", "upload:presigned"},    // value misspelt
		{"upload:presigne", "upload:presigned"},    // value truncated
		{"uplod:presigned", "upload:presigned"},    // key misspelt
		{"auto_delete:flase", "auto_delete:false"}, // the pre-existing gap
		{"upload:foo", ""},                         // too far — say nothing
		{"auto_delete:true", ""},                   // not a misspelling; the opposite
		{"enum:a|b", "enum:"},                      // free-value prefixes unchanged
		{"max_sixe:10", "max_size:"},
		{"mix:3", ""}, // a tie between min: and max: is still withheld
	} {
		if got := suggestOpt(tc.in); got != tc.want {
			t.Errorf("suggestOpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type presignOK struct {
	BaseModel
	Video string `json:"video" db:"video" mfx:"file,upload:presigned,accept:video/mp4,max_size:100"`
}

// ...and the correct spelling must register and set the flag, or the guards
// above are rejecting everything.
func TestPresignedUpload_ValidTagParses(t *testing.T) {
	meta, err := ScanModel(presignOK{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	f := meta.FieldByJSONName("video")
	if f == nil {
		t.Fatal("video field not found")
	}
	if !f.Tags.PresignedUpload {
		t.Error("PresignedUpload = false, want true")
	}
	if !f.Tags.File || f.Tags.MaxSize != 100 || len(f.Tags.Accept) != 1 {
		t.Errorf("upload:presigned disturbed its neighbours: file=%v max_size=%d accept=%v",
			f.Tags.File, f.Tags.MaxSize, f.Tags.Accept)
	}
}
