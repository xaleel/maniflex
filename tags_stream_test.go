package maniflex

// 3B.2a — registration guards and tag parsing for mfx:"upload:stream".
//
// Like upload:presigned, it is a strategy for how a file field's bytes reach
// storage, so writing it where it cannot apply is a registration error rather
// than a silent no-op — the rule v0.2.3 set for every unrecognised option.

import (
	"strings"
	"testing"
)

type streamNoFile struct {
	BaseModel
	Note string `json:"note" db:"note" mfx:"upload:stream"`
}

// upload:stream on a field that stores no file streams nothing. It parses, so the
// unknown-option check cannot catch it.
func TestStreamUpload_WithoutFileIsRejected(t *testing.T) {
	_, err := ScanModel(streamNoFile{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted upload:stream on a non-file field — a directive that does nothing, in silence")
	}
	if !strings.Contains(err.Error(), "upload:stream") || !strings.Contains(err.Error(), "file") {
		t.Errorf("error does not name the directive and the fix: %v", err)
	}
}

type streamAndPresigned struct {
	BaseModel
	Video string `json:"video" db:"video" mfx:"file,upload:presigned,upload:stream"`
}

// A field has one upload strategy; presigned and stream together is a contradiction
// that must be caught, not silently resolved to whichever the parser saw last.
func TestStreamUpload_WithPresignedIsRejected(t *testing.T) {
	_, err := ScanModel(streamAndPresigned{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted both upload:presigned and upload:stream on one field")
	}
	if !strings.Contains(err.Error(), "upload:presigned") || !strings.Contains(err.Error(), "upload:stream") {
		t.Errorf("error does not name the conflicting directives: %v", err)
	}
}

type streamFileList struct {
	BaseModel
	Clips FileKeys `json:"clips" db:"clips" mfx:"file,upload:stream"`
}

// A FileKeys list is populated by reference, not multipart, so streaming has no
// part to stream — reject rather than mount a no-op on a list.
func TestStreamUpload_OnFileListIsRejected(t *testing.T) {
	_, err := ScanModel(streamFileList{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted upload:stream on a FileKeys list")
	}
	if !strings.Contains(err.Error(), "upload:stream") {
		t.Errorf("error does not name the directive: %v", err)
	}
}

type streamOK struct {
	BaseModel
	Video string `json:"video" db:"video" mfx:"file,upload:stream,accept:video/mp4,max_size:100"`
}

// The correct spelling must register, set the flag, and leave its neighbours
// alone — or the guards above are rejecting everything.
func TestStreamUpload_ValidTagParses(t *testing.T) {
	meta, err := ScanModel(streamOK{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	f := meta.FieldByJSONName("video")
	if f == nil {
		t.Fatal("video field not found")
	}
	if !f.Tags.StreamUpload {
		t.Error("StreamUpload = false, want true")
	}
	if f.Tags.PresignedUpload {
		t.Error("PresignedUpload = true, want false — upload:stream set the wrong flag")
	}
	if !f.Tags.File || f.Tags.MaxSize != 100 || len(f.Tags.Accept) != 1 {
		t.Errorf("upload:stream disturbed its neighbours: file=%v max_size=%d accept=%v",
			f.Tags.File, f.Tags.MaxSize, f.Tags.Accept)
	}
	if !meta.hasStreamingFileField() {
		t.Error("hasStreamingFileField() = false, want true")
	}
}

// A misspelt value must still get the nearest suggestion, and neither upload value
// must be offered where it is not meant.
func TestSuggestOpt_StreamValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"upload:steam", "upload:stream"},       // value misspelt (missing r)
		{"upload:streem", "upload:stream"},      // value misspelt (vowel)
		{"upload:presined", "upload:presigned"}, // the other value still resolves right
		{"upload:foo", ""},                      // too far from either — say nothing
	} {
		if got := suggestOpt(tc.in); got != tc.want {
			t.Errorf("suggestOpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
