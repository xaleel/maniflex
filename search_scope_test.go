package maniflex

// Audit AUTH-20: searchScope decides which filters constrain a search. The two
// rules it enforces cannot be reached over HTTP — /search builds no QueryParams
// from the URL, so ctx.Query is nil unless a scope middleware creates it — which
// is why they are pinned here rather than in the e2e suite.

import (
	"strings"
	"testing"
)

func TestSearchScope_NilQueryIsNoScope(t *testing.T) {
	got, err := searchScope(nil)
	if err != nil {
		t.Fatalf("searchScope(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil — a request with no query parameters imposes no scope", got)
	}
}

// Only Forced filters scope a search. A client's own ?filter= lands in the same
// slice on every other operation and has never constrained anything the server
// did not ask for; /search does not parse one today, and this is what keeps that
// true if it ever starts.
func TestSearchScope_OnlyForcedFiltersCount(t *testing.T) {
	q := &QueryParams{Filters: []*FilterExpr{
		{Field: "tenant_id", Operator: OpEq, Value: "acme", Forced: true},
		{Field: "status", Operator: OpEq, Value: "draft"}, // a client filter
		nil,
		{Field: "owner_id", Operator: OpEq, Value: "u1", Forced: true},
	}}
	got, err := searchScope(q)
	if err != nil {
		t.Fatalf("searchScope: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d filter(s), want 2 (the forced ones): %v", len(got), got)
	}
	for _, f := range got {
		if !f.Forced {
			t.Errorf("filter on %q is not forced and must not scope a search", f.Field)
		}
		if f.Field == "status" {
			t.Error("a client's own filter reached the search scope")
		}
	}
}

// A nested-relation scope is refused, not dropped. It renders as
// "relation"."column", which needs a join the search query does not emit:
// passing it through would produce SQL referencing a table that was never
// joined, and dropping it would serve the model unscoped — the failure the
// scope exists to prevent. db.ForceFilterVia is what produces one.
func TestSearchScope_NestedForcedFilterIsRefused(t *testing.T) {
	q := &QueryParams{Filters: []*FilterExpr{{
		Field:       "item.owner_id",
		RelationKey: "item",
		NestedField: "owner_id",
		IsNested:    true,
		Operator:    OpEq,
		Value:       "u1",
		Forced:      true,
	}}}
	got, err := searchScope(q)
	if err == nil {
		t.Fatalf("a nested forced filter must be refused, got %v", got)
	}
	// The message has to name the scope and say what to do, since it surfaces as
	// a 500 on a search the operator believed was scoped.
	for _, want := range []string{"item", "owner_id", "global search"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// MUST STILL WORK: a nested filter the client sent is not forced, so it is
// ignored rather than turning every search into a 500.
func TestSearchScope_NestedClientFilterIsIgnored(t *testing.T) {
	q := &QueryParams{Filters: []*FilterExpr{{
		Field: "item.owner_id", RelationKey: "item", NestedField: "owner_id",
		IsNested: true, Operator: OpEq, Value: "u1",
	}}}
	got, err := searchScope(q)
	if err != nil {
		t.Fatalf("an unforced nested filter must be ignored, not refused: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d filter(s), want 0", len(got))
	}
}
