package e2e

// filter_field_test.go drives the *_field operators through the real HTTP
// surface on whichever driver the lane is running.
//
// It is the cross-driver assertion the feature most needs. A column-to-column
// comparison is the one filter shape where the drivers can silently disagree:
// Postgres refuses a cross-type comparison outright while SQLite orders by
// storage class and answers, so a bug in the type-class gate shows up as a 400
// on one lane and a wrong-but-plausible list on the other. Unit tests pin the
// SQL text; only this pins the rows that come back.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// FieldCmpOrder carries a filterable column of each shape the operators accept,
// plus the two that must be refused: a non-filterable numeric column and a
// string column that would be a cross-class comparison.
type FieldCmpOrder struct {
	maniflex.BaseModel
	Ref        string `json:"ref"         db:"ref"         mfx:"required,filterable"`
	AmountDue  int64  `json:"amount_due"  db:"amount_due"  mfx:"filterable,sortable"`
	PaidAmount int64  `json:"paid_amount" db:"paid_amount" mfx:"filterable,sortable"`
	Credit     *int64 `json:"credit"      db:"credit"      mfx:"filterable"`
	Note       string `json:"note"        db:"note"        mfx:"filterable"`
	Internal   int64  `json:"internal"    db:"internal"`
}

// fieldCmpServer boots a server with three orders: one underpaid, one exactly
// paid, one overpaid. Credit is NULL on all but the overpaid one.
func fieldCmpServer(t *testing.T) *testutil.Server {
	t.Helper()
	s := testutil.NewServer(t, testutil.Options{Models: []any{FieldCmpOrder{}}})

	seed := []map[string]any{
		{"ref": "under", "amount_due": 100, "paid_amount": 40, "note": "a"},
		{"ref": "exact", "amount_due": 100, "paid_amount": 100, "note": "a"},
		{"ref": "over", "amount_due": 100, "paid_amount": 150, "note": "a", "credit": 50},
	}
	for _, row := range seed {
		s.POST("/field_cmp_orders", row).AssertStatus(http.StatusCreated)
	}
	return s
}

// refs pulls the "ref" of every row in a list response, so a test can assert on
// which records matched rather than only how many.
func refs(t *testing.T, resp *testutil.Response) []string {
	t.Helper()
	var out []string
	for _, item := range resp.DataList() {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("list item is %T, want map[string]any", item)
		}
		ref, ok := row["ref"].(string)
		if !ok {
			t.Fatalf("row has no string ref: %#v", row)
		}
		out = append(out, ref)
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
		if seen[w] < 0 {
			return false
		}
	}
	return true
}

func TestFieldComparisonFiltersRows(t *testing.T) {
	s := fieldCmpServer(t)

	cases := []struct {
		name   string
		filter string
		want   []string
	}{
		{"gte_field_is_settled", "paid_amount:gte_field:amount_due", []string{"exact", "over"}},
		{"lt_field_is_outstanding", "paid_amount:lt_field:amount_due", []string{"under"}},
		{"eq_field_is_exact", "paid_amount:eq_field:amount_due", []string{"exact"}},
		{"gt_field_is_overpaid", "paid_amount:gt_field:amount_due", []string{"over"}},
		{"lte_field", "paid_amount:lte_field:amount_due", []string{"under", "exact"}},
		{"neq_field", "paid_amount:neq_field:amount_due", []string{"under", "over"}},
		// NULL on either side yields NULL, which is not true, so the row drops
		// out — of the positive and the negative form alike. That surprises
		// people often enough to pin it.
		{"null_rhs_excludes_the_row", "paid_amount:gte_field:credit", []string{"over"}},
		{"null_rhs_excludes_it_from_the_negation_too", "paid_amount:lt_field:credit", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.GET("/field_cmp_orders?filter=" + tc.filter).AssertStatus(http.StatusOK)
			got := refs(t, resp)
			if !sameSet(got, tc.want) {
				t.Errorf("?filter=%s returned %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

func TestFieldComparisonCombinesWithOtherFilters(t *testing.T) {
	s := fieldCmpServer(t)

	t.Run("ands_with_a_literal_filter", func(t *testing.T) {
		resp := s.GET(
			"/field_cmp_orders?filter=paid_amount:gte_field:amount_due&filter=ref:eq:over",
		).AssertStatus(http.StatusOK)
		if got := refs(t, resp); !sameSet(got, []string{"over"}) {
			t.Errorf("got %v, want [over]", got)
		}
	})

	t.Run("ors_within_a_group", func(t *testing.T) {
		resp := s.GET(
			"/field_cmp_orders?filter[0]=paid_amount:gt_field:amount_due&filter[0]=ref:eq:under",
		).AssertStatus(http.StatusOK)
		if got := refs(t, resp); !sameSet(got, []string{"over", "under"}) {
			t.Errorf("got %v, want [over under]", got)
		}
	})

	t.Run("meta_total_counts_the_same_rows", func(t *testing.T) {
		resp := s.GET(
			"/field_cmp_orders?filter=paid_amount:gte_field:amount_due",
		).AssertStatus(http.StatusOK)
		total, ok := resp.Meta()["total"].(float64)
		if !ok {
			t.Fatalf("meta.total is %#v, want a number", resp.Meta()["total"])
		}
		if int(total) != 2 {
			t.Errorf("meta.total = %d, want 2 — the count query and the list query disagree", int(total))
		}
	})
}

func TestFieldComparisonRejectsBadRequests(t *testing.T) {
	s := fieldCmpServer(t)

	bad := []struct {
		name   string
		filter string
	}{
		{"unknown_rhs", "paid_amount:gte_field:nope"},
		{"unknown_lhs", "nope:gte_field:paid_amount"},
		{"non_filterable_rhs", "paid_amount:gte_field:internal"},
		{"cross_class", "paid_amount:gte_field:note"},
		{"missing_rhs", "paid_amount:gte_field"},
		{"empty_rhs", "paid_amount:gte_field:"},
		{"dotted_rhs", "paid_amount:gte_field:customer.credit"},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.GET("/field_cmp_orders?filter=" + tc.filter)
			resp.AssertStatus(http.StatusBadRequest)
			if code := resp.ErrorCode(); code != "INVALID_QUERY" {
				t.Errorf("error code = %q, want INVALID_QUERY", code)
			}
		})
	}
}

func TestFieldComparisonAppearsInOpenAPI(t *testing.T) {
	s := fieldCmpServer(t)

	// The spec is not an API envelope, so read the raw bytes — resp.Data() looks
	// for a "data" key and would fail the test for the wrong reason.
	resp := s.GET("/openapi.json").AssertStatus(http.StatusOK)
	body := string(resp.Body)
	for _, op := range []string{"eq_field", "gte_field", "lte_field"} {
		if !strings.Contains(body, op) {
			t.Errorf("openapi.json does not mention %q in the filter parameter description", op)
		}
	}
}
