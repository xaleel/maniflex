package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Test models ───────────────────────────────────────────────────────────────

type M2MProduct struct {
	maniflex.BaseModel
	Name string   `json:"name" db:"name" mfx:"required,filterable,sortable"`
	Tags []M2MTag `json:"tags,omitempty" mfx:"through:M2MProductTag"`
}

type M2MTag struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required,filterable,sortable,unique"`
}

type M2MProductTag struct {
	maniflex.BaseModel
	ProductID  string    `json:"product_id"  db:"product_id"  mfx:"required,filterable,relation"`
	TagID      string    `json:"tag_id"      db:"tag_id"      mfx:"required,filterable,relation"`
	AssignedAt time.Time `json:"assigned_at" db:"assigned_at"`
	AssignedBy string    `json:"assigned_by" db:"assigned_by"`
	Product    M2MProduct
	Tag        M2MTag
}

func m2mServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{M2MProduct{}, M2MTag{}, M2MProductTag{}},
	})
}

// TestM2M_IncludeOnSingle verifies ?include=tags on a single product.
func TestM2M_IncludeOnSingle(t *testing.T) {
	t.Parallel()
	srv := m2mServer(t)

	prodID := srv.MustID(srv.POST("/m2m_products", map[string]any{"name": "Widget"}))
	tagID1 := srv.MustID(srv.POST("/m2m_tags", map[string]any{"name": "sale"}))
	tagID2 := srv.MustID(srv.POST("/m2m_tags", map[string]any{"name": "new"}))

	srv.POST("/m2m_product_tags", map[string]any{
		"product_id": prodID, "tag_id": tagID1, "assigned_by": "alice",
	}).AssertStatus(http.StatusCreated)
	srv.POST("/m2m_product_tags", map[string]any{
		"product_id": prodID, "tag_id": tagID2, "assigned_by": "bob",
	}).AssertStatus(http.StatusCreated)

	data := srv.GET(fmt.Sprintf("/m2m_products/%s?include=tags", prodID)).Data()

	tags, ok := data["tags"].([]any)
	if !ok {
		t.Fatalf("expected tags array, got %T: %v", data["tags"], data["tags"])
	}
	testutil.AssertLen(t, "included tags", tags, 2)

	// Verify _through payload is present
	for _, tag := range tags {
		m := tag.(map[string]any)
		through, ok := m["_through"].(map[string]any)
		if !ok {
			t.Errorf("expected _through on tag %v", m)
			continue
		}
		if through["assigned_by"] == nil {
			t.Errorf("expected assigned_by in _through, got %v", through)
		}
	}
}

// TestM2M_IncludeOnList verifies N+1-safe batch loading on a list endpoint.
func TestM2M_IncludeOnList(t *testing.T) {
	t.Parallel()
	srv := m2mServer(t)

	p1ID := srv.MustID(srv.POST("/m2m_products", map[string]any{"name": "Alpha"}))
	p2ID := srv.MustID(srv.POST("/m2m_products", map[string]any{"name": "Beta"}))
	tID := srv.MustID(srv.POST("/m2m_tags", map[string]any{"name": "shared"}))

	srv.POST("/m2m_product_tags", map[string]any{"product_id": p1ID, "tag_id": tID}).AssertStatus(http.StatusCreated)
	srv.POST("/m2m_product_tags", map[string]any{"product_id": p2ID, "tag_id": tID}).AssertStatus(http.StatusCreated)

	items := srv.GET("/m2m_products?include=tags").DataList()
	testutil.AssertLen(t, "products", items, 2)
	for _, item := range items {
		m := item.(map[string]any)
		tags, ok := m["tags"].([]any)
		if !ok {
			t.Errorf("product %v missing tags", m["id"])
			continue
		}
		if len(tags) != 1 {
			t.Errorf("expected 1 tag per product, got %d", len(tags))
		}
	}
}

// TestM2M_ProductWithNoTags returns empty tags slice.
func TestM2M_ProductWithNoTags(t *testing.T) {
	t.Parallel()
	srv := m2mServer(t)

	prodID := srv.MustID(srv.POST("/m2m_products", map[string]any{"name": "Lonely"}))
	data := srv.GET(fmt.Sprintf("/m2m_products/%s?include=tags", prodID)).Data()

	tags, ok := data["tags"].([]any)
	if !ok {
		// nil or missing key is also acceptable for empty
		if data["tags"] != nil {
			t.Fatalf("unexpected tags value: %v", data["tags"])
		}
		return
	}
	testutil.AssertLen(t, "empty tags", tags, 0)
}

// TestM2M_FilterOnManyToMany_Returns400 verifies that ?filter=tags.name:eq:X
// returns 400 in v1.
func TestM2M_FilterOnManyToMany_Returns400(t *testing.T) {
	t.Parallel()
	srv := m2mServer(t)
	srv.GET("/m2m_products?filter=tags.name:eq:x").AssertStatus(http.StatusBadRequest)
}
