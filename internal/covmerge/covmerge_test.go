package covmerge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) *Profile {
	t.Helper()
	p, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

// The whole point of the package: a block exercised only by the end-to-end
// suite must come out covered, and one exercised by both must not be
// double-counted into a different answer than "covered".
func TestMerge_CombinesCountsPerBlock(t *testing.T) {
	unit := parse(t, `mode: atomic
ex.com/p/a.go:1.1,2.2 3 1
ex.com/p/b.go:5.1,6.2 2 0
`)
	e2e := parse(t, `mode: atomic
ex.com/p/a.go:1.1,2.2 3 4
ex.com/p/b.go:5.1,6.2 2 0
ex.com/p/c.go:9.1,9.9 1 7
`)

	got, err := Merge(unit, e2e)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	want := map[string]int64{
		"ex.com/p/a.go:1.1,2.2": 5, // 1 + 4
		"ex.com/p/b.go:5.1,6.2": 0, // untouched by either
		"ex.com/p/c.go:9.1,9.9": 7, // e2e only — the case the gate was blind to
	}
	if len(got.Blocks) != len(want) {
		t.Fatalf("blocks: got %d, want %d", len(got.Blocks), len(want))
	}
	for _, b := range got.Blocks {
		if w, ok := want[b.Key]; !ok {
			t.Errorf("unexpected block %s", b.Key)
		} else if b.Count != w {
			t.Errorf("%s count: got %d, want %d", b.Key, b.Count, w)
		}
	}
}

// In set mode the counts are presence flags, so summing them would produce a
// number the format does not define. Covered-by-either is the only sound merge.
func TestMerge_SetModeOrsRatherThanSums(t *testing.T) {
	a := parse(t, "mode: set\nex.com/p/a.go:1.1,2.2 3 1\n")
	b := parse(t, "mode: set\nex.com/p/a.go:1.1,2.2 3 1\n")

	got, err := Merge(a, b)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.Blocks[0].Count != 1 {
		t.Errorf("set-mode count: got %d, want 1 (a flag, not a tally)", got.Blocks[0].Count)
	}
}

func TestMerge_RejectsModeMismatch(t *testing.T) {
	a := parse(t, "mode: set\nex.com/p/a.go:1.1,2.2 3 1\n")
	b := parse(t, "mode: atomic\nex.com/p/a.go:1.1,2.2 3 1\n")

	if _, err := Merge(a, b); err == nil {
		t.Fatal("merging set with atomic must fail: the counts mean different things")
	}
}

// A block whose statement count differs between profiles means they were built
// from different source. Merging would report a total for a tree that never
// existed, so it is refused rather than silently reconciled.
func TestMerge_RejectsDisagreeingStatementCounts(t *testing.T) {
	a := parse(t, "mode: atomic\nex.com/p/a.go:1.1,2.2 3 1\n")
	b := parse(t, "mode: atomic\nex.com/p/a.go:1.1,2.2 9 1\n")

	if _, err := Merge(a, b); err == nil {
		t.Fatal("disagreeing statement counts must fail")
	}
}

func TestProfile_TotalAndByPackage(t *testing.T) {
	p := parse(t, `mode: atomic
ex.com/p/one/a.go:1.1,2.2 3 1
ex.com/p/one/b.go:1.1,2.2 2 0
ex.com/p/two/c.go:1.1,2.2 5 4
`)

	if got := (p.Total()); got.Stmts != 10 || got.Covered != 8 {
		t.Errorf("Total: got %+v, want {Stmts:10 Covered:8}", got)
	}

	byPkg := p.ByPackage()
	if s := byPkg["ex.com/p/one"]; s.Stmts != 5 || s.Covered != 3 {
		t.Errorf("one: got %+v, want {Stmts:5 Covered:3}", s)
	}
	if s := byPkg["ex.com/p/two"]; s.Percent() != 100 {
		t.Errorf("two: got %.1f%%, want 100%%", s.Percent())
	}
}

// A package with no statements must not read as 0% — it would fail any floor
// forever with no way to fix it.
func TestStats_EmptyPackageIsNotAFailure(t *testing.T) {
	if got := (Stats{}).Percent(); got != 100 {
		t.Errorf("empty package: got %.1f%%, want 100%%", got)
	}
}

func TestLoadFilesAndWriteFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.out")
	b := filepath.Join(dir, "b.out")
	if err := os.WriteFile(a, []byte("mode: atomic\nex.com/p/a.go:1.1,2.2 3 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("mode: atomic\nex.com/p/a.go:1.1,2.2 3 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles, err := LoadFiles([]string{a, b})
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	merged, err := Merge(profiles...)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	out := filepath.Join(dir, "merged.out")
	if err := merged.WriteFile(out); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	const want = "mode: atomic\nex.com/p/a.go:1.1,2.2 3 3\n"
	if string(got) != want {
		t.Errorf("merged file:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestLoadFiles_MissingFileFails(t *testing.T) {
	if _, err := LoadFiles([]string{filepath.Join(t.TempDir(), "nope.out")}); err == nil {
		t.Fatal("a missing profile must fail rather than be skipped")
	}
}

func TestReport_ListsPackagesSorted(t *testing.T) {
	p := parse(t, `mode: atomic
ex.com/p/zeta/a.go:1.1,2.2 2 1
ex.com/p/alpha/b.go:1.1,2.2 2 0
`)
	var sb strings.Builder
	if err := p.Report(&sb); err != nil {
		t.Fatalf("Report: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), sb.String())
	}
	if !strings.HasPrefix(lines[0], "ex.com/p/alpha") || !strings.Contains(lines[0], "0.0%") {
		t.Errorf("first line should be alpha at 0.0%%: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "ex.com/p/zeta") || !strings.Contains(lines[1], "100.0%") {
		t.Errorf("second line should be zeta at 100.0%%: %q", lines[1])
	}
}

func TestParse_RejectsMissingModeHeader(t *testing.T) {
	if _, err := Parse(strings.NewReader("ex.com/p/a.go:1.1,2.2 3 1\n")); err == nil {
		t.Fatal("a profile without a mode header must fail")
	}
}

func TestParseFloors(t *testing.T) {
	got, err := ParseFloors(" a/b = 75 , c/d=90.5 ,, ")
	if err != nil {
		t.Fatalf("ParseFloors: %v", err)
	}
	if len(got) != 2 || got["a/b"] != 75 || got["c/d"] != 90.5 {
		t.Errorf("got %v, want {a/b:75 c/d:90.5}", got)
	}
	for _, bad := range []string{"a/b", "a/b=x"} {
		if _, err := ParseFloors(bad); err == nil {
			t.Errorf("ParseFloors(%q) should fail", bad)
		}
	}
}

func TestCheck_ReportsUnmetFloors(t *testing.T) {
	p := parse(t, `mode: atomic
ex.com/p/one/a.go:1.1,2.2 4 1
ex.com/p/one/b.go:1.1,2.2 6 0
ex.com/p/two/c.go:1.1,2.2 5 1
`) // one = 40%, two = 100%, aggregate = 60%

	if got := p.Check(50, map[string]float64{"ex.com/p/two": 90}); len(got) != 0 {
		t.Errorf("met floors should report nothing, got %v", got)
	}

	got := p.Check(80, map[string]float64{"ex.com/p/one": 75})
	if len(got) != 2 {
		t.Fatalf("want 2 failures (aggregate + one), got %d: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "aggregate") || !strings.Contains(joined, "ex.com/p/one") {
		t.Errorf("failures should name the aggregate and the package: %v", got)
	}
}

// A floor naming a package that no longer reports data must fail rather than
// silently guard nothing — that is how a gate rots.
func TestCheck_UnknownPackageIsAFailure(t *testing.T) {
	p := parse(t, "mode: atomic\nex.com/p/one/a.go:1.1,2.2 4 1\n")

	got := p.Check(0, map[string]float64{"ex.com/p/gone": 50})
	if len(got) != 1 || !strings.Contains(got[0], "ex.com/p/gone") {
		t.Errorf("want a failure naming the missing package, got %v", got)
	}
}

func TestRoundTrip(t *testing.T) {
	const src = `mode: atomic
ex.com/p/a.go:1.1,2.2 3 5
ex.com/p/b.go:5.1,6.2 2 0
`
	var sb strings.Builder
	if err := parse(t, src).Write(&sb); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sb.String() != src {
		t.Errorf("round trip:\ngot:\n%s\nwant:\n%s", sb.String(), src)
	}
}
