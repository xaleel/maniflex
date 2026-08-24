package maniflex

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// internalRequire matches a require line naming a module of this repository,
// capturing the path and the version. The trailing version is what separates a
// require from the `module` declaration, which carries none.
var internalRequire = regexp.MustCompile(
	`(github\.com/xaleel/maniflex(?:/[A-Za-z0-9._/-]+)?)\s+(v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*)`)

// TestInternalModulePinsAgree fails when one module of this repository requires
// another at a version other than the one every other module is pinned to.
//
// This is the shape of the v0.7.0 finding: the release runbook bumps each
// submodule's pin on the *core* and nothing else, so maniflextest — which
// imports db/postgres and db/sqlite — sat at v0.4.0 through three releases
// while its core pin moved to v0.7.0.
//
// Nothing in the repository could see it. go.work resolves siblings from disk,
// so every in-repo build and test ignores the pin entirely; release-dry-run.sh
// runs with GOWORK=off but v0.4.0 exists and compiles, so it passes too; and
// MVS raises the core to the newest requirement regardless, so no version
// conflict is ever reported. The only thing that noticed was an out-of-repo
// consumer resolving through the proxy — the last step of the release, run
// after the tags are already immutable.
//
// The cost is not a broken build. It is that `go get maniflextest@vX` hands a
// consumer sibling modules missing every fix since v0.4.0, among them the
// Postgres pool defaults that v0.5.1 cut from 20/40 to 3/6.
//
// This check reads go.mod files rather than building, so it holds for module
// pairs no test imports and needs no network.
func TestInternalModulePinsAgree(t *testing.T) {
	type pin struct {
		file    string
		path    string
		version string
	}
	var pins []pin

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS and dependency trees; every module of this repo is a
			// plain directory beneath the root.
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(src), "\n") {
			line = strings.TrimSpace(line)
			// The module declaration names this repo without a version, so the
			// regexp's mandatory version already excludes it; skip it anyway so
			// a future `module x v2`-style line cannot be misread as a require.
			if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "//") {
				continue
			}
			if m := internalRequire.FindStringSubmatch(line); m != nil {
				pins = append(pins, pin{file: filepath.ToSlash(path), path: m[1], version: m[2]})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking for go.mod files: %v", err)
	}

	if len(pins) == 0 {
		t.Fatal("found no internal module requirements at all; this check has stopped " +
			"reading what it is meant to read, and would pass whatever the pins said")
	}

	byVersion := map[string][]string{}
	for _, p := range pins {
		byVersion[p.version] = append(byVersion[p.version], p.file+" -> "+p.path)
	}
	if len(byVersion) == 1 {
		return
	}

	versions := make([]string, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)

	var b strings.Builder
	b.WriteString("modules of this repository are pinned to each other at more than one version.\n")
	b.WriteString("Every internal require must name the same version — the release bump has to\n")
	b.WriteString("cover sibling modules, not only the core.\n")
	for _, v := range versions {
		sites := byVersion[v]
		sort.Strings(sites)
		b.WriteString("\n  " + v + ":\n")
		for _, s := range sites {
			b.WriteString("    " + s + "\n")
		}
	}
	t.Error(b.String())
}
