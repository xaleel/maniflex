package e2e

// AG-3 — aggregate results are JSON numbers on every driver.
//
// The string-to-number conversion itself only fires on Postgres, where lib/pq
// hands back a NUMERIC as text; the unit tests in the root package drive it
// with simulated driver values, and CI's Postgres lane exercises the real path.
// What these tests pin on the SQLite lane is the half that is checkable here:
// that the endpoint emits an unquoted number for a numeric aggregate, and that
// the normalisation is scoped correctly — a MIN over a text column and a
// group_by column must keep their strings, since coercing those would be a new
// bug rather than a fix.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type NumSale struct {
	maniflex.BaseModel
	Sku    string `json:"sku"    db:"sku"    mfx:"required,filterable,sortable"`
	Amount int    `json:"amount" db:"amount" mfx:"filterable,sortable"`
}

func numSaleServer(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{
		NumSale{},
		maniflex.ModelConfig{AggregateEnabled: true},
	}})
	// "00123" is the value that matters: it is a string a numeric coercion
	// would silently rewrite as 123, losing the leading zeros.
	for _, row := range []map[string]any{
		{"sku": "00123", "amount": 100},
		{"sku": "00456", "amount": 50},
	} {
		srv.POST("/num_sales", row).AssertStatus(http.StatusCreated)
	}
	return srv
}

func aggBody(t *testing.T, srv *testutil.Server, spec string) string {
	t.Helper()
	resp := srv.GET("/num_sales/aggregate?aggregate=" + url.QueryEscape(spec))
	resp.AssertStatus(http.StatusOK)
	return string(resp.Body)
}

// A numeric aggregate is an unquoted JSON number in the body.
func TestAggregateNumber_SumIsUnquoted(t *testing.T) {
	t.Parallel()
	srv := numSaleServer(t)

	body := aggBody(t, srv, `{"select":[{"op":"sum","field":"amount","as":"total"}]}`)
	if !strings.Contains(body, `"total":150`) {
		t.Fatalf(`want "total":150 unquoted, got: %s`, body)
	}
	if strings.Contains(body, `"total":"`) {
		t.Fatalf("total came back quoted: %s", body)
	}
}

// MIN over a text column returns text, and must not be coerced — "00123" is not
// the number 123. This is what keeps the AG-3 fix from becoming its own bug.
func TestAggregateNumber_MinOverTextStaysAString(t *testing.T) {
	t.Parallel()
	srv := numSaleServer(t)

	body := aggBody(t, srv, `{"select":[{"op":"min","field":"sku","as":"first"}]}`)
	if !strings.Contains(body, `"first":"00123"`) {
		t.Fatalf(`want "first":"00123" quoted and intact, got: %s`, body)
	}
}

// A group_by column is never an aggregate result, so it is never converted
// either — the same reasoning, on the other axis of the query.
func TestAggregateNumber_GroupByColumnStaysAString(t *testing.T) {
	t.Parallel()
	srv := numSaleServer(t)

	body := aggBody(t, srv,
		`{"select":[{"op":"sum","field":"amount","as":"total"}],"group_by":["sku"]}`)
	if !strings.Contains(body, `"sku":"00123"`) {
		t.Fatalf(`want the group key to stay the string "00123", got: %s`, body)
	}
}

// COUNT is an integer on both drivers and must stay one.
func TestAggregateNumber_CountIsUnquoted(t *testing.T) {
	t.Parallel()
	srv := numSaleServer(t)

	body := aggBody(t, srv, `{"select":[{"op":"count","as":"n"}]}`)
	if !strings.Contains(body, `"n":2`) {
		t.Fatalf(`want "n":2 unquoted, got: %s`, body)
	}
}
