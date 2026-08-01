// Package covmerge merges Go coverage text profiles and reports per-package
// totals.
//
// It exists because the framework's coverage lives in two places: unit tests in
// the root module and the end-to-end suite in the separate tests module. Go
// writes one profile per `go test` invocation and ships no way to combine them,
// so measuring either alone understates the real figure badly — the root-module
// run reports 0% for packages whose entire test suite is end-to-end, and a gate
// built on that number is blind to two thirds of the coverage it claims to
// enforce.
package covmerge

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Block is one coverage-profile record: a half-open source range, the number of
// statements in it, and how many times it ran.
type Block struct {
	// Key is "<import path>/<file>:<startLine>.<startCol>,<endLine>.<endCol>",
	// verbatim from the profile. It identifies the block across profiles.
	Key   string
	Stmts int
	Count int64
}

// Profile is a parsed coverage profile.
type Profile struct {
	// Mode is "set", "count", or "atomic". Profiles only merge within a mode:
	// "set" records presence (0 or 1) while the others record hit counts, so
	// combining them would silently produce a number that means neither.
	Mode   string
	Blocks []Block
}

// Parse reads one coverage profile.
func Parse(r io.Reader) (*Profile, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	p := &Profile{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if p.Mode == "" {
			rest, ok := strings.CutPrefix(line, "mode:")
			if !ok {
				return nil, fmt.Errorf("covmerge: first line is %q, want a \"mode:\" header", line)
			}
			p.Mode = strings.TrimSpace(rest)
			continue
		}
		b, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		p.Blocks = append(p.Blocks, b)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("covmerge: read: %w", err)
	}
	if p.Mode == "" {
		return nil, fmt.Errorf("covmerge: empty profile")
	}
	return p, nil
}

// parseBlock splits "<key> <numStmts> <count>". The key itself contains no
// spaces, but splitting from the right is still the safer read: only the two
// trailing fields are fixed in shape.
func parseBlock(line string) (Block, error) {
	i := strings.LastIndexByte(line, ' ')
	if i < 0 {
		return Block{}, fmt.Errorf("covmerge: malformed line %q", line)
	}
	j := strings.LastIndexByte(line[:i], ' ')
	if j < 0 {
		return Block{}, fmt.Errorf("covmerge: malformed line %q", line)
	}
	stmts, err := strconv.Atoi(line[j+1 : i])
	if err != nil {
		return Block{}, fmt.Errorf("covmerge: statement count in %q: %w", line, err)
	}
	count, err := strconv.ParseInt(line[i+1:], 10, 64)
	if err != nil {
		return Block{}, fmt.Errorf("covmerge: hit count in %q: %w", line, err)
	}
	return Block{Key: line[:j], Stmts: stmts, Count: count}, nil
}

// Merge combines profiles, summing hit counts for blocks that appear in more
// than one. In "set" mode the counts are presence flags rather than tallies, so
// they are OR-ed instead: a block covered by either run is covered.
//
// The result is sorted by key, which is also what `go tool cover` expects.
func Merge(profiles ...*Profile) (*Profile, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("covmerge: nothing to merge")
	}

	out := &Profile{Mode: profiles[0].Mode}
	counts := make(map[string]int64)
	stmts := make(map[string]int)
	var order []string

	for _, p := range profiles {
		if p.Mode != out.Mode {
			return nil, fmt.Errorf("covmerge: mode mismatch: %q and %q — regenerate both "+
				"profiles with the same -covermode", out.Mode, p.Mode)
		}
		for _, b := range p.Blocks {
			if _, seen := counts[b.Key]; !seen {
				order = append(order, b.Key)
				stmts[b.Key] = b.Stmts
			} else if stmts[b.Key] != b.Stmts {
				return nil, fmt.Errorf("covmerge: block %s has %d statements in one profile "+
					"and %d in another — the profiles describe different source",
					b.Key, stmts[b.Key], b.Stmts)
			}
			if out.Mode == "set" {
				if b.Count > 0 {
					counts[b.Key] = 1
				}
			} else {
				counts[b.Key] += b.Count
			}
		}
	}

	sort.Strings(order)
	out.Blocks = make([]Block, 0, len(order))
	for _, k := range order {
		out.Blocks = append(out.Blocks, Block{Key: k, Stmts: stmts[k], Count: counts[k]})
	}
	return out, nil
}

// Write emits the profile in the format `go tool cover` reads. It deliberately
// is not WriteTo: that name is reserved by io.WriterTo for a (int64, error)
// signature, and vet's stdmethods check rejects the mismatch.
func (p *Profile) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "mode: %s\n", p.Mode); err != nil {
		return err
	}
	for _, b := range p.Blocks {
		if _, err := fmt.Fprintf(bw, "%s %d %d\n", b.Key, b.Stmts, b.Count); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Stats is a statement count and how many of those statements ran.
type Stats struct {
	Stmts   int
	Covered int
}

// Percent is the covered fraction, or 100 for a package with no statements —
// an empty package is not a coverage failure.
func (s Stats) Percent() float64 {
	if s.Stmts == 0 {
		return 100
	}
	return float64(s.Covered) / float64(s.Stmts) * 100
}

// Total is coverage across every block.
func (p *Profile) Total() Stats {
	var s Stats
	for _, b := range p.Blocks {
		s.Stmts += b.Stmts
		if b.Count > 0 {
			s.Covered += b.Stmts
		}
	}
	return s
}

// ByPackage groups coverage by import path, so a gate can hold individual
// packages to a floor rather than letting one well-tested package mask a
// neglected one in the aggregate.
func (p *Profile) ByPackage() map[string]Stats {
	out := make(map[string]Stats)
	for _, b := range p.Blocks {
		s := out[packageOf(b.Key)]
		s.Stmts += b.Stmts
		if b.Count > 0 {
			s.Covered += b.Stmts
		}
		out[packageOf(b.Key)] = s
	}
	return out
}

// packageOf strips the position suffix and the file name from a block key,
// leaving the import path.
func packageOf(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		key = key[:i]
	}
	return path.Dir(key)
}

// ParseFloors reads a "import/path=percent,other=percent" list.
func ParseFloors(s string) (map[string]float64, error) {
	out := map[string]float64{}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pkg, pct, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("covmerge: bad floor %q, want import/path=percent", part)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil {
			return nil, fmt.Errorf("covmerge: bad percent in %q: %w", part, err)
		}
		out[strings.TrimSpace(pkg)] = v
	}
	return out, nil
}

// Check reports every unmet floor, sorted, or nil when all are met.
func (p *Profile) Check(aggregate float64, perPackage map[string]float64) []string {
	var failures []string

	if total := p.Total(); aggregate > 0 && total.Percent() < aggregate {
		failures = append(failures, fmt.Sprintf("aggregate %.1f%% is below the %.1f%% floor",
			total.Percent(), aggregate))
	}

	byPkg := p.ByPackage()
	for pkg, min := range perPackage {
		s, ok := byPkg[pkg]
		if !ok {
			// A floor naming a package with no data would otherwise sit in the
			// config passing forever while guarding nothing.
			failures = append(failures, fmt.Sprintf(
				"%s: no coverage data — package renamed or removed?", pkg))
			continue
		}
		if s.Percent() < min {
			failures = append(failures, fmt.Sprintf(
				"%s: %.1f%% is below its %.1f%% floor (%d/%d statements)",
				pkg, s.Percent(), min, s.Covered, s.Stmts))
		}
	}

	sort.Strings(failures)
	return failures
}

// LoadFiles parses each named profile.
func LoadFiles(paths []string) ([]*Profile, error) {
	profiles := make([]*Profile, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		parsed, err := Parse(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		profiles = append(profiles, parsed)
	}
	return profiles, nil
}

// WriteFile writes the profile to path.
func (p *Profile) WriteFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := p.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Report renders per-package coverage, sorted by import path.
func (p *Profile) Report(w io.Writer) error {
	byPkg := p.ByPackage()
	names := make([]string, 0, len(byPkg))
	for k := range byPkg {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		s := byPkg[n]
		if _, err := fmt.Fprintf(w, "%-55s %6.1f%%  (%d/%d)\n",
			n, s.Percent(), s.Covered, s.Stmts); err != nil {
			return err
		}
	}
	return nil
}
