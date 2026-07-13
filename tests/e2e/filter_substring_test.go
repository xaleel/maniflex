package e2e

// ?filter=name:like:50% is parameterised, but % and _ still reach SQL as
// wildcards with no ESCAPE clause, so a filter for a literal "50%" quietly
// matched "500 units" too — and a client had no portable way to escape them
// (SQLite has no escape character by default, Postgres has a backslash). The new
// contains/starts_with/ends_with operators take a literal value (BUG-22).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type priceTag struct {
	maniflex.BaseModel
	Label string `json:"label" db:"label" mfx:"required,filterable"`
}

// labelsMatching returns the labels a filter selects, sorted by nothing in
// particular — the tests compare as sets.
func labelsMatching(t *testing.T, srv *testutil.Server, filter string) map[string]bool {
	t.Helper()
	resp := srv.GET("/price_tags?filter=" + filter)
	resp.AssertStatus(http.StatusOK)

	got := map[string]bool{}
	for _, item := range resp.DataList() {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("unexpected list item %T", item)
		}
		got[row["label"].(string)] = true
	}
	return got
}

func newSubstringServer(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{priceTag{}}})
	for _, label := range []string{"50% off", "500 units", "SALE 50% today", "a_b", "axb"} {
		srv.POST("/price_tags", map[string]any{"label": label}).
			AssertStatus(http.StatusCreated)
	}
	return srv
}

func TestFilterSubstring_ContainsTreatsPercentLiterally(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	got := labelsMatching(t, srv, "label:contains:50%25") // %25 is an encoded '%'
	want := map[string]bool{"50% off": true, "SALE 50% today": true}

	for label := range want {
		if !got[label] {
			t.Errorf("contains:50%% did not match %q", label)
		}
	}
	if got["500 units"] {
		t.Error(`contains:50% matched "500 units" — the % was still a wildcard`)
	}
	if len(got) != len(want) {
		t.Errorf("matched %d rows, want %d: %v", len(got), len(want), got)
	}
}

func TestFilterSubstring_ContainsTreatsUnderscoreLiterally(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	got := labelsMatching(t, srv, "label:contains:a_b")
	if !got["a_b"] {
		t.Error(`contains:a_b did not match "a_b"`)
	}
	if got["axb"] {
		t.Error(`contains:a_b matched "axb" — the _ was still a single-character wildcard`)
	}
}

func TestFilterSubstring_StartsWithAndEndsWith(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	if got := labelsMatching(t, srv, "label:starts_with:50%25"); !got["50% off"] || got["SALE 50% today"] || got["500 units"] {
		t.Errorf(`starts_with:50%% matched %v, want only "50%% off"`, got)
	}
	if got := labelsMatching(t, srv, "label:ends_with:today"); !got["SALE 50% today"] || len(got) != 1 {
		t.Errorf(`ends_with:today matched %v, want only "SALE 50%% today"`, got)
	}
}

// Case-insensitive: these are for user-typed text.
func TestFilterSubstring_ContainsIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	if got := labelsMatching(t, srv, "label:contains:sale"); !got["SALE 50% today"] {
		t.Errorf(`contains:sale did not match "SALE 50%% today" (matched %v)`, got)
	}
}

// like keeps its old meaning — the value is a pattern, wildcards and all. That is
// the whole reason contains exists alongside it.
func TestFilterSubstring_LikeIsStillAPattern(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	got := labelsMatching(t, srv, "label:like:50%25")
	if !got["50% off"] || !got["500 units"] {
		t.Errorf(`like:50%% should match both "50%% off" and "500 units" as a pattern; matched %v`, got)
	}
}

// A substring operator with no value at all would be a bare wildcard matching
// every row — rejected rather than silently returning the table.
func TestFilterSubstring_MissingValueRejected(t *testing.T) {
	t.Parallel()
	srv := newSubstringServer(t)

	resp := srv.GET("/price_tags?filter=label:contains")
	resp.AssertStatus(http.StatusBadRequest)
	if code := resp.ErrorCode(); code != "INVALID_QUERY" {
		t.Errorf("error code: got %q, want INVALID_QUERY", code)
	}
}
