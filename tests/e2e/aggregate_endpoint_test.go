package e2e

// 4.7 — auto-generated GET /:model/aggregate endpoint. The route is mounted only
// when ModelConfig.AggregateEnabled; it takes the aggregation spec as URL-encoded
// JSON in ?aggregate=, validates every referenced field against the model's
// filterable/sortable allow-list, and runs it through ctx.Aggregate. It
// dispatches as the list operation so list auth/tenancy middleware apply.
//
// The spec used to travel in the GET's request body, which many proxies and CDNs
// drop and fetch() cannot send at all — so the endpoint worked in development and
// failed in production (DX-2). The body is no longer read.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// AggSale is a fixture with a mix of filterable, sortable, and plain columns so
// the allow-list behaviour of the aggregate endpoint can be exercised.
type AggSale struct {
	maniflex.BaseModel
	Region  string `json:"region"  db:"region"  mfx:"filterable,sortable"`
	Product string `json:"product" db:"product" mfx:"filterable"`
	Amount  int    `json:"amount"  db:"amount"  mfx:"sortable"`
	Secret  string `json:"secret"  db:"secret"` // neither filterable nor sortable
}

// aggEndpointServer wires AggSale with AggregateEnabled and applies optional
// extra middleware.
func aggEndpointServer(t *testing.T, mw func(s *maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			AggSale{},
			maniflex.ModelConfig{AggregateEnabled: true},
		},
		Middleware: mw,
	})
}

func seedSales(t *testing.T, srv *testutil.Server) {
	t.Helper()
	type sale struct {
		region, product string
		amount          int
	}
	for _, s := range []sale{
		{"us", "A", 100},
		{"us", "B", 50},
		{"eu", "A", 200},
		{"eu", "A", 300},
		{"ap", "C", 25},
	} {
		srv.POST("/agg_sales", map[string]any{
			"region":  s.region,
			"product": s.product,
			"amount":  s.amount,
			"secret":  "x",
		}).AssertStatus(http.StatusCreated)
	}
}

// aggGET issues a GET /agg_sales/aggregate with the spec in ?aggregate=, and no
// request body at all.
func aggGET(srv *testutil.Server, spec any) *testutil.Response {
	return srv.Do(http.MethodGet, aggURL(srv, spec), nil)
}

// aggURL builds the endpoint URL, encoding spec as JSON into ?aggregate=. A nil
// or empty spec yields a bare URL — the "no spec supplied" case.
func aggURL(srv *testutil.Server, spec any) string {
	var raw string
	switch v := spec.(type) {
	case nil:
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		raw = string(b)
	}

	u := srv.APIPath("/agg_sales/aggregate")
	if raw == "" {
		return u
	}
	return u + "?aggregate=" + url.QueryEscape(raw)
}

// rowsByKey indexes a list of group rows by the string value of the given key.
func rowsByKey(t *testing.T, rows []any, key string) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("row is not an object: %T", r)
		}
		k, _ := m[key].(string)
		out[k] = m
	}
	return out
}

func TestAggregateEndpoint_NotMountedByDefault(t *testing.T) {
	t.Parallel()
	// Same model without AggregateEnabled — the route must not exist.
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{AggSale{}},
	})
	resp := aggGET(srv, map[string]any{
		"select": []any{map[string]any{"op": "count", "as": "n"}},
	})
	resp.AssertStatus(http.StatusNotFound)
}

func TestAggregateEndpoint_CountAndSumByGroup(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	resp := aggGET(srv, map[string]any{
		"select": []any{
			map[string]any{"op": "count", "as": "n"},
			map[string]any{"op": "sum", "field": "amount", "as": "total"},
		},
		"group_by": []any{"region"},
	})
	resp.AssertStatus(http.StatusOK)

	rows := resp.DataList()
	if len(rows) != 3 {
		t.Fatalf("rows: got %d, want 3 (one per region)", len(rows))
	}
	byRegion := rowsByKey(t, rows, "region")
	for _, c := range []struct {
		region string
		n      float64
		total  float64
	}{
		{"us", 2, 150},
		{"eu", 2, 500},
		{"ap", 1, 25},
	} {
		m, ok := byRegion[c.region]
		if !ok {
			t.Errorf("missing group %q", c.region)
			continue
		}
		if got := toF(m["n"]); got != c.n {
			t.Errorf("region=%q count: got %v, want %v", c.region, got, c.n)
		}
		if got := toF(m["total"]); got != c.total {
			t.Errorf("region=%q total: got %v, want %v", c.region, got, c.total)
		}
	}
}

func TestAggregateEndpoint_WhereFiltersRows(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	// Only product=A rows: us(100) + eu(200) + eu(300) = 600 across 3 rows.
	resp := aggGET(srv, map[string]any{
		"select": []any{
			map[string]any{"op": "count", "as": "n"},
			map[string]any{"op": "sum", "field": "amount", "as": "total"},
		},
		"where": []any{
			map[string]any{"field": "product", "operator": "eq", "value": "A"},
		},
	})
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	m := rows[0].(map[string]any)
	if got := toF(m["n"]); got != 3 {
		t.Errorf("count: got %v, want 3", got)
	}
	if got := toF(m["total"]); got != 600 {
		t.Errorf("total: got %v, want 600", got)
	}
}

func TestAggregateEndpoint_HavingFiltersGroups(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	// Only groups whose total > 100: us(150) and eu(500); ap(25) is dropped.
	resp := aggGET(srv, map[string]any{
		"select": []any{
			map[string]any{"op": "sum", "field": "amount", "as": "total"},
		},
		"group_by": []any{"region"},
		"having": []any{
			map[string]any{"alias": "total", "operator": "gt", "value": 100},
		},
	})
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	byRegion := rowsByKey(t, rows, "region")
	if _, ok := byRegion["ap"]; ok {
		t.Errorf("group ap should be filtered out by HAVING")
	}
}

func TestAggregateEndpoint_OrderByAndLimit(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	// Order by total desc, limit 1 → the eu group (500).
	resp := aggGET(srv, map[string]any{
		"select": []any{
			map[string]any{"op": "sum", "field": "amount", "as": "total"},
		},
		"group_by": []any{"region"},
		"order_by": []any{
			map[string]any{"field": "total", "direction": "desc"},
		},
		"limit": 1,
	})
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	m := rows[0].(map[string]any)
	if m["region"] != "eu" {
		t.Errorf("region: got %v, want eu", m["region"])
	}
	if got := toF(m["total"]); got != 500 {
		t.Errorf("total: got %v, want 500", got)
	}
}

func TestAggregateEndpoint_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body any
	}{
		{
			"no spec at all",
			nil,
		},
		{
			"empty spec",
			[]byte(""),
		},
		{
			"malformed json",
			[]byte("{not json"),
		},
		{
			"no select",
			map[string]any{"group_by": []any{"region"}},
		},
		{
			"unknown op",
			map[string]any{"select": []any{map[string]any{"op": "median", "field": "amount"}}},
		},
		{
			"unknown field",
			map[string]any{"select": []any{map[string]any{"op": "sum", "field": "nope"}}},
		},
		{
			// secret exists but is neither filterable nor sortable → rejected.
			"non-allowlisted field",
			map[string]any{"select": []any{map[string]any{"op": "max", "field": "secret"}}},
		},
		{
			"non-allowlisted group_by",
			map[string]any{
				"select":   []any{map[string]any{"op": "count", "as": "n"}},
				"group_by": []any{"secret"},
			},
		},
		{
			"unsupported where operator",
			map[string]any{
				"select": []any{map[string]any{"op": "count", "as": "n"}},
				"where": []any{
					map[string]any{"field": "amount", "operator": "between", "value": "1,2"},
				},
			},
		},
		{
			"unknown json key",
			map[string]any{
				"select":  []any{map[string]any{"op": "count", "as": "n"}},
				"groupby": []any{"region"}, // typo: should be group_by
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := aggEndpointServer(t, nil)
			resp := aggGET(srv, tc.body)
			if resp.Status != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400\nbody: %s", resp.Status, resp.Body)
			}
			if code := resp.ErrorCode(); code != "INVALID_AGGREGATE" {
				t.Errorf("error code: got %q, want INVALID_AGGREGATE", code)
			}
		})
	}
}

// The whole point of DX-2: a spec sent the old way — in the GET's body — is not
// honoured. It has to be rejected rather than silently obeyed, because the body
// is exactly what a CDN or ALB strips on the way to production; a request that
// works here and 400s there is the failure mode being fixed.
func TestAggregateEndpoint_GetBodyIsNotRead(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	spec := map[string]any{
		"select": []any{map[string]any{"op": "count", "as": "n"}},
	}
	resp := srv.Do(http.MethodGet, srv.APIPath("/agg_sales/aggregate"), spec)

	resp.AssertStatus(http.StatusBadRequest)
	if code := resp.ErrorCode(); code != "INVALID_AGGREGATE" {
		t.Errorf("error code: got %q, want INVALID_AGGREGATE", code)
	}
	// The error has to tell the caller where the spec belongs now.
	if msg := resp.Body; !strings.Contains(string(msg), "?aggregate=") {
		t.Errorf("error message does not point at ?aggregate=: %s", msg)
	}
}

// And the query parameter is what the endpoint actually reads, even when a body
// is also present — no silent precedence to argue about.
func TestAggregateEndpoint_QueryParamWinsOverBody(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, nil)
	seedSales(t, srv)

	// The body asks to group by product; the query parameter asks for a flat count.
	body := map[string]any{
		"select":   []any{map[string]any{"op": "count", "as": "n"}},
		"group_by": []any{"product"},
	}
	url := aggURL(srv, map[string]any{
		"select": []any{map[string]any{"op": "count", "as": "n"}},
	})

	resp := srv.Do(http.MethodGet, url, body)
	resp.AssertStatus(http.StatusOK)

	rows := resp.DataList()
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1 — the body's group_by must not have been applied", len(rows))
	}
	if got := toF(rows[0].(map[string]any)["n"]); got != 5 {
		t.Errorf("count: got %v, want 5", got)
	}
}

// TestAggregateEndpoint_ListAuthApplies proves the spec guarantee: auth
// middleware registered for OpList also protects the aggregate endpoint, with
// no OpAggregate-specific registration.
func TestAggregateEndpoint_ListAuthApplies(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Abort(http.StatusForbidden, "FORBIDDEN", "no list access")
			return nil
		}, maniflex.ForModel("AggSale"), maniflex.ForOperation(maniflex.OpList))
	})

	resp := aggGET(srv, map[string]any{
		"select": []any{map[string]any{"op": "count", "as": "n"}},
	})
	resp.AssertStatus(http.StatusForbidden)
}

// TestAggregateEndpoint_TenancyFilterFolded proves that filters injected into
// ctx.Query.Filters by list middleware (e.g. tenancy force-filters) are AND-ed
// into the aggregate WHERE.
func TestAggregateEndpoint_TenancyFilterFolded(t *testing.T) {
	t.Parallel()
	srv := aggEndpointServer(t, func(s *maniflex.Server) {
		// Runs after Deserialize (so ctx.Query exists) and before the DB step.
		s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
				Field: "region", Operator: maniflex.OpEq, Value: "us", Group: -1,
			})
			return next()
		}, maniflex.ForModel("AggSale"), maniflex.ForOperation(maniflex.OpList))
	})
	seedSales(t, srv)

	// No where in the body — the only constraint is the injected region=us filter.
	resp := aggGET(srv, map[string]any{
		"select": []any{
			map[string]any{"op": "count", "as": "n"},
			map[string]any{"op": "sum", "field": "amount", "as": "total"},
		},
	})
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	m := rows[0].(map[string]any)
	if got := toF(m["n"]); got != 2 {
		t.Errorf("count: got %v, want 2 (us rows only)", got)
	}
	if got := toF(m["total"]); got != 150 {
		t.Errorf("total: got %v, want 150 (us rows only)", got)
	}
}
