package maniflex_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex"
)

// minimal stubs so we can call ParseQueryParams without a real registry

type stubRegistry struct{}

func (s stubRegistry) Get(name string) (*maniflex.ModelMeta, bool) {
	if name == "Author" {
		return authorMeta(), true
	}
	return nil, false
}

func (s stubRegistry) All() []*maniflex.ModelMeta { return nil }

func postMeta() *maniflex.ModelMeta {
	return &maniflex.ModelMeta{
		Name:      "Post",
		TableName: "posts",
		Fields: []maniflex.FieldMeta{
			{Name: "Status", Tags: maniflex.FieldTags{DBName: "status", JSONName: "status", Filterable: true}},
			{Name: "Title", Tags: maniflex.FieldTags{DBName: "title", JSONName: "title", Filterable: true}},
		},
		Relations: []maniflex.RelationMeta{
			{
				RelationKey:  "author",
				RelatedModel: "Author",
				Kind:         maniflex.BelongsTo,
				FKColumn:     "author_id",
			},
		},
	}
}

func authorMeta() *maniflex.ModelMeta {
	return &maniflex.ModelMeta{
		Name:      "Author",
		TableName: "authors",
		Fields: []maniflex.FieldMeta{
			{Name: "Role", Tags: maniflex.FieldTags{DBName: "role", JSONName: "role", Filterable: true}},
		},
	}
}

func parseWithKeys(t *testing.T, rawURL string) (*maniflex.QueryParams, error) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("bad url: %v", err)
	}
	r := &http.Request{URL: u}
	return maniflex.ParseQueryParams(r, postMeta(), stubRegistry{})
}

// ── parser: group assignment ──────────────────────────────────────────────────

func TestFilterParser_UnbracketedIsUngrouped(t *testing.T) {
	q, err := parseWithKeys(t, "http://x/?filter=status:eq:draft")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Filters) != 1 {
		t.Fatalf("want 1 filter, got %d", len(q.Filters))
	}
	if q.Filters[0].Group != -1 {
		t.Fatalf("ungrouped filter should have Group=-1, got %d", q.Filters[0].Group)
	}
}

func TestFilterParser_BracketedGroupAssigned(t *testing.T) {
	q, err := parseWithKeys(t, "http://x/?filter%5B0%5D=status:eq:draft&filter%5B0%5D=status:eq:published")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Filters) != 2 {
		t.Fatalf("want 2 filters, got %d", len(q.Filters))
	}
	// URL group index N maps to internal Group N+1, leaving Group<=0 (incl. the
	// zero value) reserved for "ungrouped/AND". So filter[0] → internal Group 1.
	for _, f := range q.Filters {
		if f.Group != 1 {
			t.Fatalf("expected Group=1 (filter[0]+1), got %d", f.Group)
		}
	}
}

func TestFilterParser_MixedBracketedAndUnbracketed(t *testing.T) {
	// filter[0]=status:eq:draft  &  filter[0]=status:eq:published  &  filter=title:eq:Hello
	q, err := parseWithKeys(t, "http://x/?filter%5B0%5D=status:eq:draft&filter%5B0%5D=status:eq:published&filter=title:eq:Hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Filters) != 3 {
		t.Fatalf("want 3 filters, got %d", len(q.Filters))
	}
	grouped, ungrouped := 0, 0
	for _, f := range q.Filters {
		if f.Group >= 1 { // filter[0] → internal Group 1
			grouped++
		} else if f.Group <= 0 { // unbracketed → ungrouped (-1)
			ungrouped++
		}
	}
	if grouped != 2 || ungrouped != 1 {
		t.Fatalf("want 2 grouped and 1 ungrouped; got grouped=%d ungrouped=%d", grouped, ungrouped)
	}
}

func TestFilterParser_InvalidBracketIndexReturns400(t *testing.T) {
	_, err := parseWithKeys(t, "http://x/?filter%5Bbad%5D=status:eq:draft")
	if err == nil {
		t.Fatal("expected error for non-digit bracket index")
	}
}

func TestFilterParser_CrossTableORGroupRejected(t *testing.T) {
	// filter[0]=status:eq:draft (primary table)  +  filter[0]=author.role:eq:admin (related table)
	_, err := parseWithKeys(t, "http://x/?filter%5B0%5D=status:eq:draft&filter%5B0%5D=author.role:eq:admin")
	if err == nil {
		t.Fatal("expected error for cross-table OR group")
	}
}

func TestFilterParser_DifferentGroupIndicesAreIndependent(t *testing.T) {
	q, err := parseWithKeys(t, "http://x/?filter%5B0%5D=status:eq:draft&filter%5B1%5D=title:eq:Hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Filters) != 2 {
		t.Fatalf("want 2, got %d", len(q.Filters))
	}
	groups := map[int]bool{}
	for _, f := range q.Filters {
		groups[f.Group] = true
	}
	// filter[0] → internal Group 1, filter[1] → internal Group 2.
	if !groups[1] || !groups[2] {
		t.Fatalf("expected internal groups 1 and 2, got %v", groups)
	}
}
