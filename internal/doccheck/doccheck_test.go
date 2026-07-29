package doccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAcceptsContextualFragments(t *testing.T) {
	root := testRepository(t, `
# Fragments

`+"```go"+`
server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),
    maniflex.ForOperation(maniflex.OpCreate),
)
`+"```"+`

`+"```go"+`
type Post struct {
    Title string `+"`json:\"title\"`"+`
}
`+"```"+`

`+"```go"+`
Middleware: []MiddlewareFunc{
    authenticate,
},
`+"```"+`
`)

	result, issues := Check(root)
	if len(issues) != 0 {
		t.Fatalf("Check issues: %v", issues)
	}
	if result.GoFences != 3 {
		t.Fatalf("GoFences = %d, want 3", result.GoFences)
	}
}

func TestCheckRejectsInvalidAndUnclosedFences(t *testing.T) {
	root := testRepository(t, `
`+"```go"+`
server := )
`+"```"+`

`+"```go"+`
unclosed()
`)

	_, issues := Check(root)
	if len(issues) != 2 {
		t.Fatalf("issues = %v, want two", issues)
	}
	if !strings.Contains(issues[0].Message, "invalid Go example") {
		t.Fatalf("first issue = %q", issues[0].Message)
	}
	if issues[1].Message != "unclosed Go code fence" {
		t.Fatalf("second issue = %q", issues[1].Message)
	}
}

func TestCheckRequiresReasonForIgnoredPseudocode(t *testing.T) {
	root := testRepository(t, `
<!-- doccheck:ignore -->
`+"```go"+`
call(<placeholder>)
`+"```"+`

<!-- doccheck:ignore reason="placeholder names are supplied by the application" -->
`+"```go"+`
call(<placeholder>)
`+"```"+`
`)

	result, issues := Check(root)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want one", issues)
	}
	if !strings.Contains(issues[0].Message, "requires a non-empty reason") {
		t.Fatalf("issue = %q", issues[0].Message)
	}
	if result.Ignored != 1 {
		t.Fatalf("Ignored = %d, want 1", result.Ignored)
	}
}

func TestCheckExpandsAnchoredGoInclude(t *testing.T) {
	root := testRepository(t, `
`+"```go"+`
{{#include ../../example_test.go:secured-docs}}
`+"```"+`
`)
	writeFile(t, filepath.Join(root, "example_test.go"), `
package example

func example() {
    // ANCHOR: secured-docs
    server := New(Config{Protected: true})
    _ = server
    // ANCHOR_END: secured-docs
}
`)

	result, issues := Check(root)
	if len(issues) != 0 {
		t.Fatalf("Check issues: %v", issues)
	}
	if result.Includes != 1 {
		t.Fatalf("Includes = %d, want 1", result.Includes)
	}
}

func TestCheckRejectsNonGoIncludeInGoFence(t *testing.T) {
	root := testRepository(t, `
`+"```go"+`
{{#include ../../example.txt}}
`+"```"+`
`)
	writeFile(t, filepath.Join(root, "example.txt"), "validCall()\n")

	_, issues := Check(root)
	if len(issues) != 1 {
		t.Fatalf("issues = %v, want one", issues)
	}
	if !strings.Contains(issues[0].Message, "compiled .go file") {
		t.Fatalf("issue = %q", issues[0].Message)
	}
}

func testRepository(t *testing.T, markdown string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "src", "test.md"), markdown)
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
