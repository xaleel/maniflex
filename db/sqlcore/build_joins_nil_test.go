package sqlcore

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// buildJoins asked every filter whether it was a relation filter, dereferencing
// it to find out, so a single nil in the list took the process down. That was
// the list-path half of audit O1 — and the half the audit did not name, though
// it is the one a real application reaches: a middleware appends whatever its
// conditional filter-builder returned, and sometimes that is nil.
//
// The framework refuses a nil at its own boundary now, with an error naming
// where it came from. This test exists because that boundary shields this loop
// from every end-to-end test: sqlcore is an importable package an adapter can
// drive directly, so the loop has to tolerate a nil on its own rather than
// trust a caller it cannot see.
func TestBuildJoinsSkipsNilFilters(t *testing.T) {
	model := &maniflex.ModelMeta{Name: "Post", TableName: "posts"}
	filters := []*maniflex.FilterExpr{
		nil,
		{
			IsNested:      true,
			RelationKey:   "author",
			RelationTable: "users",
			RelationFK:    "user_id",
			Field:         "name",
		},
		nil,
	}

	got := buildJoins(model, filters, nil)

	// The nils are ignored; the real nested filter still gets its join, so the
	// tolerance is not achieved by dropping the whole list.
	if !strings.Contains(got, "users") || !strings.Contains(got, "author") {
		t.Errorf("nested filter lost its join: %q", got)
	}
	if strings.Count(got, "LEFT JOIN") != 1 {
		t.Errorf("want exactly one join, got %q", got)
	}
}
