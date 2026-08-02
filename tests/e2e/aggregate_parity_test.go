package e2e

// AG-1/AG-4 — the aggregate endpoint and the list endpoint are two separate
// WHERE builders (maniflex's aggBuildWhere and db/sqlcore's filterConds), and
// every capability one grew that the other did not showed up as a silently
// wrong number: a count of zero where the list returned rows, on an endpoint
// whose entire job is reporting numbers.
//
// This is the differential test that makes the next such drift fail here rather
// than ship. Each case runs the SAME ?filter= through both endpoints and
// requires the count to equal the number of rows listed. A new operator, or a
// new filter feature, belongs in this table.

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ParityTicket struct {
	maniflex.BaseModel
	Title    string `json:"title"    db:"title"    mfx:"required,filterable,sortable"`
	Owner    string `json:"owner"    db:"owner"    mfx:"filterable"`
	Amount   int    `json:"amount"   db:"amount"   mfx:"filterable,sortable"`
	Resolved bool   `json:"resolved" db:"resolved" mfx:"filterable,sortable"`
}

func parityServer(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{
		ParityTicket{},
		maniflex.ModelConfig{AggregateEnabled: true},
	}})
	for _, row := range []map[string]any{
		{"title": "alpha", "owner": "u1", "amount": 10, "resolved": false},
		{"title": "beta", "owner": "u2", "amount": 20, "resolved": false},
		{"title": "gamma", "owner": "u1", "amount": 30, "resolved": true},
		{"title": "delta", "owner": "u3", "amount": 40, "resolved": true},
	} {
		srv.POST("/parity_tickets", row).AssertStatus(http.StatusCreated)
	}
	return srv
}

// aggregateCount runs the count aggregate with the given raw query string and
// returns the single n it reports.
func aggregateCount(t *testing.T, srv *testutil.Server, query string) int {
	t.Helper()
	spec := url.QueryEscape(`{"select":[{"op":"count","as":"n"}]}`)
	resp := srv.GET("/parity_tickets/aggregate?aggregate=" + spec + "&" + query)
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) == 0 {
		return 0
	}
	row, _ := rows[0].(map[string]any)
	n, _ := row["n"].(float64)
	return int(n)
}

func TestAggregateListParity(t *testing.T) {
	t.Parallel()
	srv := parityServer(t)

	cases := []struct {
		name  string
		query string
	}{
		{"eq", "filter=owner:eq:u1"},
		{"neq", "filter=owner:neq:u1"},
		{"gt", "filter=amount:gt:15"},
		{"gte", "filter=amount:gte:20"},
		{"lt", "filter=amount:lt:35"},
		{"lte", "filter=amount:lte:20"},
		{"in", "filter=owner:in:u1,u2"},
		{"not_in", "filter=owner:not_in:u1"},
		{"contains", "filter=title:contains:a"},
		{"starts_with", "filter=title:starts_with:al"},
		{"ends_with", "filter=title:ends_with:a"},
		{"ilike", "filter=title:ilike:%25a%25"},

		// AG-4: between had no case in the aggregate builder and fell through
		// to "=" against the raw "lo,hi" string.
		{"between", "filter=amount:between:15,35"},

		// AG-1: filters sharing a group OR in the list path and used to AND here.
		{"or_group", "filter[0]=owner:eq:u1&filter[0]=owner:eq:u2"},
		{"or_group_and_ungrouped", "filter=resolved:eq:false&filter[0]=owner:eq:u1&filter[0]=owner:eq:u2"},
		{"two_or_groups", "filter[0]=owner:eq:u1&filter[0]=owner:eq:u2&filter[1]=amount:gte:20"},

		// QF-3's aggregate half: the bool coercion the list path gained.
		{"bool_word", "filter=resolved:eq:false"},
		{"bool_word_true", "filter=resolved:eq:true"},
		{"bool_numeral", "filter=resolved:eq:0"},
		{"bool_in", "filter=resolved:in:true,false"},

		// Several filters ANDed, the ordinary case.
		{"multi_and", "filter=owner:eq:u1&filter=resolved:eq:false"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := srv.GET("/parity_tickets?limit=100&" + tc.query)
			list.AssertStatus(http.StatusOK)
			want := len(list.DataList())

			if got := aggregateCount(t, srv, tc.query); got != want {
				t.Fatalf("%s: aggregate counted %d, list returned %d rows\n  ?%s",
					tc.name, got, want, tc.query)
			}
		})
	}
}

// The parity table is only meaningful if its cases actually select something.
// A filter matching every row (or no row) would pass parity while both sides
// ignored it, which is exactly the failure being guarded against.
func TestAggregateParityCasesAreDiscriminating(t *testing.T) {
	t.Parallel()
	srv := parityServer(t)
	for _, query := range []string{
		"filter=owner:eq:u1",
		"filter=amount:between:15,35",
		"filter[0]=owner:eq:u1&filter[0]=owner:eq:u2",
		"filter=resolved:eq:false",
	} {
		n := aggregateCount(t, srv, query)
		if n == 0 || n == 4 {
			t.Fatalf("?%s counted %d of 4 rows — not a discriminating case", query, n)
		}
	}
}

// A forced filter is the dangerous case: an aggregate is a total, so a scope
// that silently stops applying reports another tenant's numbers rather than an
// error. The scope must survive alongside a client's own OR group.
func TestAggregateForcedFilterSurvivesORGroup(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			ParityTicket{},
			maniflex.ModelConfig{AggregateEnabled: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Query != nil {
					ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
						Field: "owner", Operator: maniflex.OpEq, Value: "u1",
						Group: -1, Forced: true,
					})
				}
				return next()
			})
		},
	})
	for _, row := range []map[string]any{
		{"title": "alpha", "owner": "u1", "amount": 10, "resolved": false},
		{"title": "beta", "owner": "u2", "amount": 20, "resolved": false},
		{"title": "gamma", "owner": "u1", "amount": 30, "resolved": true},
	} {
		srv.POST("/parity_tickets", row).AssertStatus(http.StatusCreated)
	}

	// The client asks for owner u1 OR u2; the forced scope pins owner to u1.
	// Two rows are u1, so the scoped answer is 2 — not 3.
	query := "filter[0]=owner:eq:u1&filter[0]=owner:eq:u2"
	if got := aggregateCount(t, srv, query); got != 2 {
		t.Fatalf("forced filter did not survive the OR group: counted %d, want 2", got)
	}
}
