// Command doccheck validates Go examples embedded in repository documentation.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/xaleel/maniflex/internal/doccheck"
)

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	result, issues := doccheck.Check(*root)
	for _, issue := range issues {
		fmt.Fprintln(os.Stderr, issue)
	}
	if len(issues) > 0 {
		fmt.Fprintf(os.Stderr, "doccheck failed: %d issue(s) across %d Go fence(s)\n", len(issues), result.GoFences)
		os.Exit(1)
	}
	fmt.Printf(
		"doccheck passed: %d Go fence(s) in %d file(s), %d compiled include(s), %d explicit ignore(s)\n",
		result.GoFences, result.Files, result.Includes, result.Ignored,
	)
}
