// Command covmerge merges Go coverage profiles and enforces coverage floors.
//
// The framework's coverage is split across two modules — unit tests in the root
// module, the end-to-end suite in tests/ — and Go cannot combine their profiles.
// Gating on either alone measures a fraction of the real figure.
//
//	covmerge -out merged.out -floor 80 -pkg-floor 'pkg=75,other=90' unit.out e2e.out
//
// Exits non-zero when a floor is unmet, listing every package that missed. The
// work lives in internal/covmerge so it can be tested; this stays a wrapper.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/xaleel/maniflex/internal/covmerge"
)

func main() {
	out := flag.String("out", "", "write the merged profile here (optional)")
	floor := flag.Float64("floor", 0, "minimum aggregate coverage percent")
	pkgFloors := flag.String("pkg-floor", "", "per-package minimums, e.g. 'import/path=75,other=90'")
	verbose := flag.Bool("v", false, "print per-package coverage")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "covmerge: no profiles given")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Args(), *out, *floor, *pkgFloors, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "covmerge:", err)
		os.Exit(1)
	}
}

func run(paths []string, out string, floor float64, pkgFloors string, verbose bool) error {
	floors, err := covmerge.ParseFloors(pkgFloors)
	if err != nil {
		return err
	}
	profiles, err := covmerge.LoadFiles(paths)
	if err != nil {
		return err
	}
	merged, err := covmerge.Merge(profiles...)
	if err != nil {
		return err
	}
	if out != "" {
		if err := merged.WriteFile(out); err != nil {
			return err
		}
	}
	if verbose {
		if err := merged.Report(os.Stdout); err != nil {
			return err
		}
	}

	total := merged.Total()
	fmt.Printf("merged %d profile(s): %.1f%% of %d statements\n",
		len(profiles), total.Percent(), total.Stmts)

	if failures := merged.Check(floor, floors); len(failures) > 0 {
		return fmt.Errorf("coverage floors unmet:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}
