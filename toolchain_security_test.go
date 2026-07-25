package maniflex

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const minimumSecureGoVersion = "1.25.12"

var goVersionDirective = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+\.[0-9]+(?:\.[0-9]+)?)[ \t]*$`)
var chiRequirement = regexp.MustCompile(`(?m)^[ \t]*github\.com/go-chi/chi/v5[ \t]+v([0-9]+\.[0-9]+\.[0-9]+)`)

func TestWorkspaceRequiresSecureDependencyVersions(t *testing.T) {
	t.Parallel()

	files := []string{"go.work"}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("find workspace manifests: %v", err)
	}

	for _, path := range files {
		path := path
		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			match := goVersionDirective.FindSubmatch(data)
			if match == nil {
				t.Fatalf("%s has no Go version directive", path)
			}
			version := string(match[1])
			if compareGoVersions(version, minimumSecureGoVersion) < 0 {
				t.Errorf("%s requires Go %s; minimum secure version is %s", path, version, minimumSecureGoVersion)
			}
			if match := chiRequirement.FindSubmatch(data); match != nil {
				version := string(match[1])
				if compareGoVersions(version, "5.3.0") < 0 {
					t.Errorf("%s requires chi v%s; minimum secure version is v5.3.0", path, version)
				}
			}
		})
	}
}

func compareGoVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for len(leftParts) < 3 {
		leftParts = append(leftParts, "0")
	}
	for len(rightParts) < 3 {
		rightParts = append(rightParts, "0")
	}

	for i := range 3 {
		leftNumber, _ := strconv.Atoi(leftParts[i])
		rightNumber, _ := strconv.Atoi(rightParts[i])
		if leftNumber < rightNumber {
			return -1
		}
		if leftNumber > rightNumber {
			return 1
		}
	}
	return 0
}
