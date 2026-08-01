package e2e

// End-to-end coverage for ModelConfig.BaseModelTags. BaseModel's columns
// default to mfx:"readonly" and nothing more, so a model's query surface over
// id / created_at / updated_at is exactly what its config opts into.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TightDoc takes the defaults: readonly BaseModel columns, no query surface.
type TightDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

// WideDoc opts created_at and id back into the query surface.
type WideDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

func baseTagsServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			TightDoc{},
			WideDoc{}, maniflex.ModelConfig{
				BaseModelTags: map[string]string{
					"id":         "filterable,sortable",
					"created_at": "filterable,sortable",
				},
			},
		},
	})
}

func TestBaseModelTags_SortRejectedByDefault(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	srv.POST("/tight_docs", map[string]any{"name": "a"}).AssertStatus(http.StatusCreated)

	srv.GET("/tight_docs?sort=created_at:desc").AssertStatus(http.StatusBadRequest)
	srv.GET("/tight_docs?filter=created_at:gte:2000-01-01").AssertStatus(http.StatusBadRequest)
}

func TestBaseModelTags_SortAllowedWhenOptedIn(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	srv.POST("/wide_docs", map[string]any{"name": "a"}).AssertStatus(http.StatusCreated)
	srv.POST("/wide_docs", map[string]any{"name": "b"}).AssertStatus(http.StatusCreated)

	srv.GET("/wide_docs?sort=created_at:desc").AssertStatus(http.StatusOK)
	srv.GET("/wide_docs?sort=id:asc").AssertStatus(http.StatusOK)
	srv.GET("/wide_docs?filter=created_at:gte:2000-01-01").AssertStatus(http.StatusOK)
}

// id is readonly now, so a client-supplied id is stripped rather than honoured.
func TestBaseModelTags_ClientSuppliedIDIsIgnored(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	resp := srv.POST("/tight_docs", map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "name": "a",
	})
	resp.AssertStatus(http.StatusCreated)
	if got := resp.ID(); got == "11111111-1111-1111-1111-111111111111" {
		t.Error(`id is mfx:"readonly" — a client-supplied id must not be honoured`)
	}
}
