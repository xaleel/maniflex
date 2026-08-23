package maniflex

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// identifierFields names the struct fields that carry a bare SQL identifier —
// a table or column name as the database spells it, with no quoting applied.
//
// Passing one of these straight into a statement is the shape of audit O6:
// BackfillRollups interpolated cr.onDB and child.TableName raw, which the
// migrator had created quoted. It broke on a camelCase column (Postgres created
// parentId case-sensitively and could not find it as parentid) and on a reserved
// word, while SQLite folded identifier case and hid the first of those entirely.
var identifierFields = []string{
	"TableName",
	"DBName",
	"onDB",
	"RelationTable",
	"RelationFK",
	"RelationKey",
	"NestedField",
	"CursorField",
	"Field",
	"ParentField",
	"Column",
}

// sqlFormatMarkers are the fragments that mark a format string as SQL rather
// than prose. Requiring one keeps the check off error messages and log lines,
// which name the same identifiers legitimately and often.
var sqlFormatMarkers = []string{
	"SELECT ", "INSERT INTO", "UPDATE ", "DELETE FROM",
	"CREATE TABLE", "ALTER TABLE", " FROM ", " WHERE ", " JOIN ",
}

// A forgotten Quote is invisible in review, and — because SQLite folds
// identifier case where Postgres does not — invisible on the default local test
// lane too. It surfaces in production, on whichever statement nobody ran against
// Postgres. That is how O6 shipped.
//
// This walks every shipped .go file for a fmt.Sprintf that builds SQL and
// receives one of identifierFields directly, rather than through Quote/q. It
// catches the shape in code no test drives, which is the point: the class hides
// exactly where coverage does not reach.
//
// What it cannot do is prove absence. It reasons about the shape of an argument
// at one call site, so an identifier laundered through a local variable or a
// helper that forgets to quote internally still passes. It narrows the class
// rather than closing it.
func TestSQLIdentifiersAreQuoted(t *testing.T) {
	skipDir := map[string]bool{
		".git": true, "docs": true, "todo": true, "book": true,
		"scripts": true, "examples": true, "tests": true, "internal": true,
	}

	var findings []string
	scanned := 0

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
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil // the build polices syntax, not this test
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isFmtSprintf(call.Fun) || len(call.Args) < 2 {
				return true
			}
			format, ok := stringLit(call.Args[0])
			if !ok || !looksLikeSQL(format) {
				return true
			}
			scanned++
			for _, arg := range call.Args[1:] {
				if fieldName, ok := bareIdentifierField(arg); ok {
					findings = append(findings, fmt.Sprintf("%s: %s passed unquoted into %q",
						fset.Position(arg.Pos()), fieldName, truncate(format, 60)))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Without this the test passes just as happily when the walk finds nothing
	// at all, which is the failure mode a scanner is most likely to develop.
	if scanned == 0 {
		t.Fatal("found no SQL-building Sprintf calls at all — the walk is broken, not the code")
	}

	t.Logf("scanned %d SQL-building fmt.Sprintf call(s)", scanned)

	if len(findings) > 0 {
		slices.Sort(findings)
		t.Fatalf("%d SQL identifier(s) interpolated without Quote:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

func isFmtSprintf(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, "`\""), true
}

func looksLikeSQL(format string) bool {
	upper := strings.ToUpper(format)
	for _, m := range sqlFormatMarkers {
		if strings.Contains(upper, m) {
			return true
		}
	}
	return false
}

// bareIdentifierField reports whether e is a direct field access naming a raw
// identifier — model.TableName, f.Tags.DBName, cr.onDB. A Quote(...) or q(...)
// call is a CallExpr and never matches, and neither does a local already holding
// a quoted value, which is the common `table := Quote(m.TableName)` idiom.
func bareIdentifierField(e ast.Expr) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if slices.Contains(identifierFields, sel.Sel.Name) {
		return sel.Sel.Name, true
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
