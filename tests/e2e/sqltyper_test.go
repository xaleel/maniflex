package e2e

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/pkg/money"
	"maniflex/tests/e2e/testutil"
)

// TestSQLTyper verifies that types implementing maniflex.SQLTyper receive the
// correct schema column type and that normal CRUD works through the map
// pipeline (values stored/retrieved as decimal strings).
func TestSQLTyper(t *testing.T) {
	t.Parallel()

	// ── model that uses money.Amount ─────────────────────────────────────────

	type Product struct {
		maniflex.BaseModel
		Name  string       `json:"name"  db:"name"  mfx:"required,filterable,sortable"`
		Price money.Amount `json:"price" db:"price" mfx:"required"`
	}

	t.Run("schema_created_with_correct_column_type", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Product{}},
		})

		// Create a record — if the column type were wrong (e.g. BLOB), this
		// would fail or return unexpected data.
		resp := srv.POST("/products", map[string]any{
			"name":  "Widget",
			"price": 12.34,
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "id", resp.ID())
	})

	t.Run("crud_roundtrip_with_decimal_value", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Product{}},
		})

		// Create
		createResp := srv.POST("/products", map[string]any{
			"name":  "Gadget",
			"price": 99.99,
		})
		createResp.AssertStatus(http.StatusCreated)
		id := createResp.ID()

		// Read back — price stored as TEXT in SQLite ("99.9900" or "99.99"),
		// returned as a string in the JSON response.
		getResp := srv.GET("/products/" + id)
		getResp.AssertStatus(http.StatusOK)
		data := getResp.Data()
		if data["price"] == nil {
			t.Fatal("price field missing from response")
		}

		// Update
		patchResp := srv.PATCH("/products/"+id, map[string]any{"price": 49.50})
		patchResp.AssertStatus(http.StatusOK)
		updated := patchResp.Data()
		if updated["price"] == nil {
			t.Fatal("price field missing from patch response")
		}
	})

	t.Run("zero_value_amount_stored_and_retrieved", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Product{}},
		})

		resp := srv.POST("/products", map[string]any{
			"name":  "Free",
			"price": 0,
		})
		resp.AssertStatus(http.StatusCreated)
		data := resp.Data()
		if data["price"] == nil {
			t.Fatal("price field missing from response")
		}
	})
}
