// Package doccheck validates Go examples embedded in the documentation.
package doccheck

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	goFenceRE  = regexp.MustCompile(`^\s*` + "```" + `(?:go|golang)(?:[\s,].*)?$`)
	fenceEndRE = regexp.MustCompile(`^\s*` + "```" + `\s*$`)
	ignoreRE   = regexp.MustCompile(`^\s*<!--\s*doccheck:ignore(?:\s+reason="([^"]*)")?\s*-->\s*$`)
	includeRE  = regexp.MustCompile(`\{\{#include\s+([^\s}:]+)(?::([^\s}]+))?\s*\}\}`)
)

// Result summarizes one documentation check.
type Result struct {
	Files    int
	GoFences int
	Includes int
	Ignored  int
}

// Issue identifies a documentation validation failure.
type Issue struct {
	Path    string
	Line    int
	Message string
}

func (i Issue) String() string {
	return fmt.Sprintf("%s:%d: %s", filepath.ToSlash(i.Path), i.Line, i.Message)
}

// Check validates Markdown under docs/src plus the repository README and
// CONTRIBUTING guide. Go fragments are syntax-checked in several legal
// contexts; mdBook includes are expanded before checking.
func Check(root string) (Result, []Issue) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Result{}, []Issue{{Path: root, Line: 1, Message: err.Error()}}
	}

	var paths []string
	docsRoot := filepath.Join(root, "docs", "src")
	err = filepath.WalkDir(docsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return Result{}, []Issue{{Path: docsRoot, Line: 1, Message: err.Error()}}
	}
	for _, name := range []string{"README.md", "CONTRIBUTING.md"} {
		path := filepath.Join(root, name)
		if _, statErr := os.Stat(path); statErr == nil {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	var result Result
	var issues []Issue
	for _, path := range paths {
		result.Files++
		fileResult, fileIssues := checkFile(root, path)
		result.GoFences += fileResult.GoFences
		result.Includes += fileResult.Includes
		result.Ignored += fileResult.Ignored
		issues = append(issues, fileIssues...)
	}
	return result, issues
}

func checkFile(root, path string) (Result, []Issue) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, []Issue{{Path: relative(root, path), Line: 1, Message: err.Error()}}
	}

	lines := splitLines(data)
	var result Result
	var issues []Issue
	for i := 0; i < len(lines); i++ {
		if !goFenceRE.MatchString(lines[i]) {
			continue
		}

		result.GoFences++
		fenceLine := i + 1
		end := i + 1
		for end < len(lines) && !fenceEndRE.MatchString(lines[end]) {
			end++
		}
		if end == len(lines) {
			issues = append(issues, Issue{
				Path: relative(root, path), Line: fenceLine,
				Message: "unclosed Go code fence",
			})
			break
		}

		code := strings.Join(lines[i+1:end], "\n")
		ignore, ignoreIssue := ignoreDirective(lines, i)
		if ignoreIssue != "" {
			issues = append(issues, Issue{
				Path: relative(root, path), Line: fenceLine, Message: ignoreIssue,
			})
		} else if ignore {
			result.Ignored++
		} else {
			expanded, includes, expandErr := expandIncludes(root, path, code)
			result.Includes += includes
			if expandErr != nil {
				issues = append(issues, Issue{
					Path: relative(root, path), Line: fenceLine, Message: expandErr.Error(),
				})
			} else if syntaxErr := validateGo(expanded); syntaxErr != nil {
				issues = append(issues, Issue{
					Path: relative(root, path), Line: fenceLine,
					Message: "invalid Go example: " + syntaxErr.Error(),
				})
			}
		}
		i = end
	}
	return result, issues
}

func ignoreDirective(lines []string, fenceIndex int) (bool, string) {
	if fenceIndex == 0 {
		return false, ""
	}
	match := ignoreRE.FindStringSubmatch(lines[fenceIndex-1])
	if match == nil {
		return false, ""
	}
	if strings.TrimSpace(match[1]) == "" {
		return false, `doccheck:ignore requires a non-empty reason="..."`
	}
	return true, ""
}

func expandIncludes(root, markdownPath, code string) (string, int, error) {
	matches := includeRE.FindAllStringSubmatchIndex(code, -1)
	if len(matches) == 0 {
		if strings.Contains(code, "{{#include") {
			return "", 0, errors.New("malformed mdBook include")
		}
		return code, 0, nil
	}

	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(code[last:match[0]])
		includePath := code[match[2]:match[3]]
		anchor := ""
		if match[4] >= 0 {
			anchor = code[match[4]:match[5]]
		}

		resolved := filepath.Clean(filepath.Join(filepath.Dir(markdownPath), filepath.FromSlash(includePath)))
		if !within(root, resolved) {
			return "", 0, fmt.Errorf("mdBook include escapes repository: %s", includePath)
		}
		if !strings.EqualFold(filepath.Ext(resolved), ".go") {
			return "", 0, fmt.Errorf("go fence include must reference a compiled .go file: %s", includePath)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return "", 0, fmt.Errorf("read mdBook include %s: %w", includePath, err)
		}
		included := string(data)
		if anchor != "" {
			included, err = anchoredRegion(included, anchor)
			if err != nil {
				return "", 0, fmt.Errorf("mdBook include %s:%s: %w", includePath, anchor, err)
			}
		}
		out.WriteString(included)
		last = match[1]
	}
	out.WriteString(code[last:])
	return out.String(), len(matches), nil
}

func anchoredRegion(source, anchor string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(source))
	startMarker := "ANCHOR: " + anchor
	endMarker := "ANCHOR_END: " + anchor
	var lines []string
	foundStart := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.Contains(line, startMarker):
			if foundStart {
				return "", errors.New("duplicate start anchor")
			}
			foundStart = true
		case strings.Contains(line, endMarker):
			if !foundStart {
				return "", errors.New("end anchor precedes start anchor")
			}
			return strings.Join(lines, "\n"), nil
		case foundStart:
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if !foundStart {
		return "", errors.New("start anchor not found")
	}
	return "", errors.New("end anchor not found")
}

func validateGo(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("empty Go code fence")
	}

	candidates := []string{
		code,
		"package docs\n" + code,
		"package docs\nfunc _() {\n" + code + "\n}",
		"package docs\ntype _ struct {\n" + code + "\n}",
		"package docs\nvar _ = (\n" + code + "\n)",
		"package docs\nvar _ = struct{}{\n" + code + "\n}",
	}
	if imports, body, ok := splitLeadingImports(code); ok {
		candidates = append(candidates,
			"package docs\n"+imports+"\nfunc _() {\n"+body+"\n}",
		)
	}

	var preferred error
	for index, candidate := range candidates {
		_, err := parser.ParseFile(token.NewFileSet(), "example.go", candidate, parser.AllErrors)
		if err == nil {
			return nil
		}
		if preferred == nil || preferredCandidate(code, index) {
			preferred = err
		}
	}
	return preferred
}

func preferredCandidate(code string, index int) bool {
	first := strings.Fields(code)
	if len(first) == 0 {
		return false
	}
	switch first[0] {
	case "package":
		return index == 0
	case "import", "const", "type", "var", "func":
		return index == 1
	default:
		return index == 2
	}
}

func splitLeadingImports(code string) (imports, body string, ok bool) {
	lines := strings.Split(code, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	start := i
	for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "import ") {
		line := strings.TrimSpace(lines[i])
		i++
		if line == "import (" {
			for i < len(lines) && strings.TrimSpace(lines[i]) != ")" {
				i++
			}
			if i == len(lines) {
				return "", "", false
			}
			i++
		}
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
	}
	if i == start {
		return "", "", false
	}
	imports = strings.Join(lines[start:i], "\n")
	body = strings.Join(lines[i:], "\n")
	if strings.TrimSpace(body) == "" {
		return "", "", false
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "imports.go", "package docs\n"+imports, parser.ImportsOnly|parser.AllErrors); err != nil {
		return "", "", false
	}
	return imports, body, true
}

func splitLines(data []byte) []string {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	return strings.Split(string(data), "\n")
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
