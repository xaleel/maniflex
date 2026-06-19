package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/money"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// EchoItem carries a money.Amount column (sql.Scanner + maniflex.SQLTyper),
// stored as text but serialised as a JSON object.
type EchoItem struct {
	maniflex.BaseModel
	Name  string       `json:"name" mfx:"filterable"`
	Price money.Amount `json:"price"`
}

// Create and update responses must echo a structured SQLTyper column as a JSON
// object — matching the typed read path — not as the raw stored string.
// Regression for the map-based create/update echo returning JSON/SQLTyper
// columns as strings (while reads returned objects).
func TestCreateUpdateEcho_StructuredColumnIsObject(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{EchoItem{}}})

	created := srv.POST("/echo_items", map[string]any{
		"name":  "Widget",
		"price": map[string]any{"amount": 1234, "currency": "USD"},
	})
	created.AssertStatus(http.StatusCreated)
	cd := created.Data()
	assertPriceObject(t, "create echo", cd["price"])

	id, _ := cd["id"].(string)
	if id == "" {
		t.Fatalf("create response missing id: %v", cd)
	}

	updated := srv.PATCH("/echo_items/"+id, map[string]any{
		"price": map[string]any{"amount": 5000, "currency": "USD"},
	})
	updated.AssertStatus(http.StatusOK)
	assertPriceObject(t, "update echo", updated.Data()["price"])

	// Control: the typed read path already returned an object — the echo must
	// now agree with it.
	got := srv.GET("/echo_items/" + id).Data()
	assertPriceObject(t, "read", got["price"])
}

func assertPriceObject(t *testing.T, where string, price any) {
	t.Helper()
	if s, isStr := price.(string); isStr {
		t.Fatalf("%s: price echoed as string %q, want a JSON object", where, s)
	}
	m, ok := price.(map[string]any)
	if !ok {
		t.Fatalf("%s: price is %T, want a JSON object: %v", where, price, price)
	}
	if _, ok := m["amount"]; !ok {
		t.Fatalf("%s: price object missing 'amount' key: %v", where, m)
	}
}
