package maniflex

import (
	"os"
	"strings"
	"testing"
)

// The README Quickstart is the first code anyone runs, and it is the one
// example doccheck cannot vouch for: fences are syntax-checked, never
// type-checked, so a field that no longer exists parses cleanly and ships. That
// is how AutoMigrate survived in the headline example for a release after being
// replaced by DisableAutoMigrate (audit D2).
//
// examples/quickstart/main.go is that example as real code, compiled by the
// examples module. This test is the other half: the compiler proves the example
// builds, and this proves the README still shows it.
func TestREADMEQuickstartMatchesCompiledExample(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	source, err := os.ReadFile("examples/quickstart/main.go")
	if err != nil {
		t.Fatalf("read examples/quickstart/main.go: %v", err)
	}

	// Trailing blank lines are the one permitted difference: gofmt requires a
	// blank line between the closing brace and the ANCHOR_END comment, and the
	// Markdown fence has no such line. Everything else must match exactly.
	documented := strings.TrimRight(quickstartFence(t, string(readme)), "\n\t ")
	compiled := strings.TrimRight(anchored(t, string(source), "quickstart"), "\n\t ")

	if documented != compiled {
		t.Fatalf("the README Quickstart has drifted from examples/quickstart/main.go, "+
			"which is the copy the compiler checks.\n\nREADME:\n%s\n\ncompiled:\n%s",
			documented, compiled)
	}
}

// quickstartFence returns the body of the first Go fence under "## Quickstart".
func quickstartFence(t *testing.T, readme string) string {
	t.Helper()

	lines := strings.Split(strings.ReplaceAll(readme, "\r\n", "\n"), "\n")
	heading := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Quickstart" {
			heading = i
			break
		}
	}
	if heading < 0 {
		t.Fatal("README.md has no '## Quickstart' section")
	}

	for i := heading + 1; i < len(lines); i++ {
		switch {
		case strings.HasPrefix(lines[i], "## "):
			t.Fatal("the README Quickstart section contains no Go fence")
		case strings.TrimSpace(lines[i]) != "```go":
			continue
		}
		for end := i + 1; end < len(lines); end++ {
			if strings.TrimSpace(lines[end]) == "```" {
				return strings.Join(lines[i+1:end], "\n")
			}
		}
		t.Fatal("the README Quickstart Go fence is never closed")
	}
	t.Fatal("the README Quickstart section contains no Go fence")
	return ""
}

// anchored returns the lines between the ANCHOR markers, matching how doccheck
// expands an mdBook include.
func anchored(t *testing.T, source, anchor string) string {
	t.Helper()

	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, "ANCHOR: "+anchor) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("examples/quickstart/main.go has no 'ANCHOR: %s' marker", anchor)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.Contains(lines[i], "ANCHOR_END: "+anchor) {
			return strings.Join(lines[start+1:i], "\n")
		}
	}
	t.Fatalf("examples/quickstart/main.go has no 'ANCHOR_END: %s' marker", anchor)
	return ""
}
