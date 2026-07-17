package maniflex

// R10 — registration guards for file columns and mfx:"max_count".
//
// The interesting failure here is not the loud one. A bare []string field fails
// at AutoMigrate ("no SQL column mapping"), and the documented way around that
// error is to wrap it in a named SQLTyper — at which point the field migrates,
// parses mfx:"file", and is skipped by every file path in silence, because each
// asserts .(string) and falls through with no else. So the workaround for a loud
// error produces a field that looks protected and enforces nothing.

import (
	"encoding/json"
	"strings"
	"testing"
)

// sqlTyperKeys is the exact workaround the AutoMigrate error recommends: a named
// type over []string implementing SQLTyper. It is the shape that used to migrate
// cleanly and then have every file rule silently waived.
type sqlTyperKeys []string

func (sqlTyperKeys) SQLType(DriverType) string { return "TEXT" }

type badFileFieldModel struct {
	BaseModel
	Images sqlTyperKeys `json:"images" db:"images" mfx:"file"`
}

func TestFileField_SQLTyperSliceIsRejected(t *testing.T) {
	_, err := ScanModel(badFileFieldModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted mfx:\"file\" on a SQLTyper slice — it migrates, parses, " +
			"and then has existence/max_size/accept/file_acl/auto_delete all skipped in silence")
	}
	if !strings.Contains(err.Error(), "FileKeys") {
		t.Errorf("error must name the supported alternative (maniflex.FileKeys): %v", err)
	}
}

type intFileFieldModel struct {
	BaseModel
	Doc int `json:"doc" db:"doc" mfx:"file"`
}

func TestFileField_NonStringIsRejected(t *testing.T) {
	_, err := ScanModel(intFileFieldModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted mfx:\"file\" on an int field")
	}
}

type stringFileModel struct {
	BaseModel
	Doc string `json:"doc" db:"doc" mfx:"file"`
}

type fileKeysModel struct {
	BaseModel
	Images FileKeys `json:"images" db:"images" mfx:"file,max_count:5"`
}

// The two supported shapes must both still register.
func TestFileField_StringAndFileKeysAreAccepted(t *testing.T) {
	if _, err := ScanModel(stringFileModel{}, ModelConfig{}); err != nil {
		t.Errorf("a string file field must register: %v", err)
	}
	if _, err := ScanModel(fileKeysModel{}, ModelConfig{}); err != nil {
		t.Errorf("a FileKeys file field must register: %v", err)
	}
}

// ── max_count ───────────────────────────────────────────────────────────────

type badMaxCountModel struct {
	BaseModel
	Images FileKeys `json:"images" db:"images" mfx:"file,max_count:1O"` // letter O
}

// max_count is protective, so a typo must not pass as a wider cap than written.
// min:/max: swallow a bad value; this one cannot.
func TestMaxCount_MalformedIsRejected(t *testing.T) {
	_, err := ScanModel(badMaxCountModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted mfx:\"max_count:1O\" — swallowing it would widen the cap " +
			"from 10 to the default 100, in silence")
	}
	if !strings.Contains(err.Error(), "max_count") {
		t.Errorf("error must name the option: %v", err)
	}
}

type maxCountOnScalarModel struct {
	BaseModel
	Doc string `json:"doc" db:"doc" mfx:"file,max_count:5"`
}

func TestMaxCount_OnSingleKeyFieldIsRejected(t *testing.T) {
	_, err := ScanModel(maxCountOnScalarModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel accepted max_count on a single-key file field, where it bounds nothing")
	}
}

// max_count must stay a known prefix option so the unknown-option checker does
// not "suggest" something else for it.
func TestMaxCount_IsAKnownOption(t *testing.T) {
	if got := tagsFor(t, "max_count:5"); len(got.UnknownOpts) > 0 {
		t.Errorf("max_count: reported unknown: %v", got.UnknownOpts)
	}
	if got := tagsFor(t, "max_count:5"); got.MaxCount != 5 {
		t.Errorf("MaxCount: got %d, want 5", got.MaxCount)
	}
}

// ── FileKeys round trip ─────────────────────────────────────────────────────

func TestFileKeys_ValueScanRoundTrip(t *testing.T) {
	in := FileKeys{"uploads/a.jpg", "uploads/b.jpg"}
	v, err := in.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var out FileKeys
	if err := out.Scan(v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("round trip: got %v, want %v", out, in)
	}
}

// Empty stores as "[]", not NULL, so a scanned column is always a valid array.
func TestFileKeys_EmptyValueIsEmptyArrayNotNull(t *testing.T) {
	v, err := FileKeys(nil).Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v != "[]" {
		t.Errorf("nil FileKeys stored as %#v, want \"[]\"", v)
	}
}

func TestFileKeys_ScanNullAndBytes(t *testing.T) {
	var fk FileKeys
	if err := fk.Scan(nil); err != nil || fk != nil {
		t.Errorf("Scan(nil): got %v, %v", fk, err)
	}
	if err := fk.Scan([]byte(`["x"]`)); err != nil || len(fk) != 1 || fk[0] != "x" {
		t.Errorf("Scan([]byte): got %v, %v", fk, err)
	}
	if err := fk.Scan(42); err == nil {
		t.Error("Scan(int) must fail rather than silently yield an empty list")
	}
}

func TestFileKeys_MarshalsAsJSONArray(t *testing.T) {
	b, err := json.Marshal(FileKeys{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["a","b"]` {
		t.Errorf("JSON: got %s, want [\"a\",\"b\"]", b)
	}
}

// ── fileKeysOfColumn ────────────────────────────────────────────────────────

// The normaliser the four file paths share. A stored FileKeys column read back
// through a map path is its JSON text, which must not be mistaken for a key.
func TestFileKeysOfColumn(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"empty string", "", nil},
		{"single key", "uploads/a.jpg", []string{"uploads/a.jpg"}},
		{"FileKeys", FileKeys{"a", "b"}, []string{"a", "b"}},
		{"stored JSON text", `["a","b"]`, []string{"a", "b"}},
		{"empty JSON array", `[]`, nil},
		{"[]any", []any{"a"}, []string{"a"}},
		{"unrecognised", 42, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fileKeysOfColumn(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// A storage key is a path and never starts with '[', so the JSON-array sniff
// cannot swallow a real key.
func TestLooksLikeJSONArray(t *testing.T) {
	for _, s := range []string{`[`, ` [`, `["a"]`, "\n[]"} {
		if !looksLikeJSONArray(s) {
			t.Errorf("looksLikeJSONArray(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "uploads/a.jpg", "a[b]", "x"} {
		if looksLikeJSONArray(s) {
			t.Errorf("looksLikeJSONArray(%q) = true, want false", s)
		}
	}
}
