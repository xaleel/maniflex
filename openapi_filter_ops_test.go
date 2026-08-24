package maniflex

// The ?filter= parameter's description enumerates the operators, and that list
// is the only place a client learns which ones exist. It was written by hand,
// next to a map of the operators the builder implements, with nothing tying the
// two together — so an operator added to one and not the other is invisible
// until somebody reads both.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestOpenAPIFilterDescriptionListsEveryOperator(t *testing.T) {
	s := New(Config{DisableAutoMigrate: true})
	if err := s.Register(jtValid{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	spec := GenerateSpec(s.Registry(), &Config{}, nil)

	desc := filterParamDescription(t, spec)
	head, rest, ok := strings.Cut(desc, "Operators: ")
	if !ok {
		t.Fatalf("filter description has no operator list:\n%s", head)
	}
	list, _, ok := strings.Cut(rest, ". ")
	if !ok {
		t.Fatalf("operator list is unterminated:\n%s", rest)
	}

	var documented []string
	for part := range strings.SplitSeq(list, ",") {
		if p := strings.TrimSpace(part); p != "" {
			documented = append(documented, p)
		}
	}
	var implemented []string
	for op := range validOperators {
		implemented = append(implemented, string(op))
	}
	sort.Strings(documented)
	sort.Strings(implemented)

	if !reflect.DeepEqual(documented, implemented) {
		t.Errorf("the ?filter= description and the operator set disagree:\n"+
			"documented: %v\nimplemented: %v\nmissing from the docs: %v\ndocumented but not implemented: %v",
			documented, implemented,
			missingFrom(implemented, documented), missingFrom(documented, implemented))
	}
}

func missingFrom(all, have []string) []string {
	seen := map[string]bool{}
	for _, h := range have {
		seen[h] = true
	}
	var out []string
	for _, a := range all {
		if !seen[a] {
			out = append(out, a)
		}
	}
	return out
}

func filterParamDescription(t *testing.T, spec *OpenAPISpec) string {
	t.Helper()
	for _, item := range spec.Paths {
		if item.Get == nil {
			continue
		}
		for _, p := range item.Get.Parameters {
			if p.Name == "filter" {
				return p.Description
			}
		}
	}
	t.Fatal("no ?filter= parameter in the generated spec")
	return ""
}
