package maniflex

import (
	"strings"
	"testing"
)

// TestSanitizeFilename pins the stricter rules introduced for roadmap §11C.2.
// Pre-fix the function only stripped `/`, `\`, and NUL — so `..`, control
// characters, and unbounded length all survived.
func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty maps to placeholder", "", "unnamed"},
		{"dot only maps to placeholder", ".", "unnamed"},
		{"double dot only maps to placeholder", "..", "unnamed"},
		{"leading dots are stripped", "...hidden.txt", "hidden.txt"},
		{"forward slash flattened", "a/b/c.txt", "a_b_c.txt"},
		{"backslash flattened", `a\b\c.txt`, "a_b_c.txt"},
		{"CR LF flattened", "a\r\nb.txt", "a__b.txt"},
		{"NUL flattened", "a\x00b.txt", "a_b.txt"},
		{"unicode flattened", "naïve.txt", "na_ve.txt"},
		{"safe chars survive", "Report_2026-05-25.pdf", "Report_2026-05-25.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFilename(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeFilename(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeFilename_TruncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := sanitizeFilename(long)
	if len(got) != maxFilenameLen {
		t.Errorf("got length %d, want %d", len(got), maxFilenameLen)
	}
}

func TestSanitizeFilename_StripsLeadingDotsFromHostileNames(t *testing.T) {
	// A real-world attack surface: `..` and friends embedded at the start of
	// a filename can become hidden files in storage. Strip them.
	cases := []string{
		"....htaccess",
		". .start",
		"..​.start",
	}
	for _, in := range cases {
		got := sanitizeFilename(in)
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeFilename(%q) = %q, must not start with '.'", in, got)
		}
	}
}
