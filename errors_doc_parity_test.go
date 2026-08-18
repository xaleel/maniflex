package maniflex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The compatibility policy promises that sentinel errors keep their identity and
// stay errors.Is-comparable for all of v1, which makes the set of them part of
// the contract. A contract nobody can read is not much of one: errors.md
// documented two of them, and the only way to find the rest was to grep.
//
// This walks the published packages for exported Err identifiers and fails when
// the page omits one, so a new sentinel cannot ship undocumented.
func TestErrorsDocListsEveryExportedSentinel(t *testing.T) {
	page, err := os.ReadFile("docs/src/the-request-pipeline/errors.md")
	if err != nil {
		t.Fatalf("read errors.md: %v", err)
	}
	doc := string(page)

	var missing []string
	for _, e := range exportedErrorIdents(t) {
		if !strings.Contains(doc, e) {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("errors.md does not mention %d exported error identifier(s): %s",
			len(missing), strings.Join(missing, ", "))
	}
}

// exportedErrorIdents returns every exported top-level identifier named Err* in
// the packages this repository publishes, sorted and deduplicated.
func exportedErrorIdents(t *testing.T) []string {
	t.Helper()

	// Not published, or not part of anyone's error contract.
	skipDir := map[string]bool{
		".git": true, "docs": true, "todo": true, "examples": true,
		"tests": true, "internal": true, "book": true, "scripts": true,
	}

	var found []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil // not our business to police syntax; the build does that
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.VAR && gen.Tok != token.TYPE) {
				continue
			}
			for _, spec := range gen.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if isExportedErrIdent(n.Name) {
							found = append(found, n.Name)
						}
					}
				case *ast.TypeSpec:
					if isExportedErrIdent(s.Name.Name) {
						found = append(found, s.Name.Name)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	slices.Sort(found)
	found = slices.Compact(found)
	if len(found) == 0 {
		t.Fatal("found no exported error identifiers at all — the walk is broken, not the docs")
	}
	return found
}

// isExportedErrIdent matches Err followed by an upper-case letter, so ErrNotFound
// counts and a hypothetical "Errors" helper does not.
func isExportedErrIdent(name string) bool {
	return strings.HasPrefix(name, "Err") && len(name) > 3 &&
		name[3] >= 'A' && name[3] <= 'Z'
}
