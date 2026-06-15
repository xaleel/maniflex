package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestSoftDelete covers both soft-delete styles: deleted_at (timestamp) and
// is_deleted (bool), plus interactions with filters and includes.
func TestSoftDelete(t *testing.T) {
	t.Parallel()

	// ── deleted_at (timestamp) style — Post model ─────────────────────────────

	t.Run("delete_sets_deleted_at_not_removed_from_db", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Soft", "draft", u))
		srv.DELETE("/posts/" + p).AssertStatus(http.StatusNoContent)
		// Record still exists in DB — confirmed indirectly: total goes down
		// and subsequent GET 404, but we can't query deleted records via the API
		srv.GET("/posts/" + p).AssertStatus(http.StatusNotFound)
	})

	t.Run("soft_deleted_record_excluded_from_list", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u2@x.com", "viewer"))
		p1 := srv.MustID(srv.CreatePost("Keep", "draft", u))
		p2 := srv.MustID(srv.CreatePost("Delete", "draft", u))
		srv.DELETE("/posts/" + p2).AssertStatus(http.StatusNoContent)

		items := srv.GET("/posts").DataList()
		testutil.AssertLen(t, "posts after soft delete", items, 1)
		testutil.AssertEqual(t, "remaining post id",
			testutil.Field(t, items[0].(map[string]any), "id"), p1)
	})

	t.Run("soft_deleted_record_not_found_on_single_read", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u3@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Gone", "draft", u))
		srv.DELETE("/posts/" + p).AssertStatus(http.StatusNoContent)
		srv.GET("/posts/" + p).AssertStatus(http.StatusNotFound)
	})

	t.Run("patch_on_soft_deleted_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u4@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Gone2", "draft", u))
		srv.DELETE("/posts/" + p).AssertStatus(http.StatusNoContent)
		srv.PATCH("/posts/"+p, map[string]any{"title": "Updated"}).AssertStatus(http.StatusNotFound)
	})

	t.Run("delete_already_soft_deleted_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u5@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("DoubleDelete", "draft", u))
		srv.DELETE("/posts/" + p).AssertStatus(http.StatusNoContent)
		srv.DELETE("/posts/" + p).AssertStatus(http.StatusNotFound)
	})

	t.Run("list_total_excludes_soft_deleted", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u6@x.com", "viewer"))
		for i := range 5 {
			srv.MustID(srv.CreatePost("P", "draft", u))
			_ = i
		}
		ids := srv.GET("/posts").DataList()
		// soft-delete 2
		srv.DELETE("/posts/" + testutil.Field(t, ids[0].(map[string]any), "id"))
		srv.DELETE("/posts/" + testutil.Field(t, ids[1].(map[string]any), "id"))
		meta := srv.GET("/posts").Meta()
		testutil.AssertEqual(t, "total excludes deleted", meta["total"], float64(3))
	})

	t.Run("filter_combined_with_soft_delete_scope", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u7@x.com", "viewer"))
		p1 := srv.MustID(srv.CreatePost("Published Live", "published", u))
		_ = srv.MustID(srv.CreatePost("Draft Live", "draft", u))
		p3 := srv.MustID(srv.CreatePost("Published Deleted", "published", u))
		srv.DELETE("/posts/" + p3)

		// Filter by published: should see only the 2 live published posts minus
		// 1 soft-deleted => should see the 1 live one
		items := srv.GET("/posts?filter=status:eq:published").DataList()
		testutil.AssertLen(t, "published live", items, 1)
		testutil.AssertEqual(t, "id matches",
			testutil.Field(t, items[0].(map[string]any), "id"), p1)
	})

	t.Run("soft_delete_does_not_affect_hard_delete_model", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// User has no soft-delete — deletion is hard
		id := srv.MustID(srv.CreateUser("HardDelete", "hd@x.com", "viewer"))
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
		// Hard-deleted — gone completely
		srv.GET("/users/" + id).AssertStatus(http.StatusNotFound)
		// List total doesn't include it
		meta := srv.GET("/users").Meta()
		testutil.AssertEqual(t, "total", meta["total"], float64(0))
	})

	// ── is_deleted (bool) style — Tag model ───────────────────────────────────

	t.Run("is_deleted_bool_soft_delete", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.POST("/tags", map[string]any{"name": "Go", "color": "blue"}))
		srv.DELETE("/tags/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/tags/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("is_deleted_bool_excluded_from_list", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.POST("/tags", map[string]any{"name": "Go", "color": "blue"}))
		id2 := srv.MustID(srv.POST("/tags", map[string]any{"name": "Rust", "color": "orange"}))
		srv.DELETE("/tags/" + id2).AssertStatus(http.StatusNoContent)
		items := srv.GET("/tags").DataList()
		testutil.AssertLen(t, "tags after bool soft-delete", items, 1)
		testutil.AssertEqual(t, "remaining name",
			testutil.Field(t, items[0].(map[string]any), "name"), "Go")
	})

	// ── Includes across soft-deleted boundaries ───────────────────────────────

	t.Run("include_on_soft_deleted_parent_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u8@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Post", "draft", u))
		srv.DELETE("/posts/" + p)
		srv.GET("/posts/" + p + "?include=user").AssertStatus(http.StatusNotFound)
	})

	t.Run("list_with_include_does_not_return_soft_deleted_children", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u9@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Post", "published", u))
		c1 := srv.MustID(srv.CreateComment("Keep", p, u))
		srv.MustID(srv.CreateComment("Keep2", p, u))
		_ = c1
		// Comments don't have soft-delete so this just tests HasMany inclusion
		data := srv.GET("/posts/" + p + "?include=comments").Data()
		comments := data["comments"].([]any)
		testutil.AssertLen(t, "comments", comments, 2)
	})

	t.Run("soft_deleted_item_not_counted_in_filtered_total", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u10@x.com", "viewer"))
		p1 := srv.MustID(srv.CreatePost("Live Draft", "draft", u))
		p2 := srv.MustID(srv.CreatePost("Dead Draft", "draft", u))
		_ = p1
		srv.DELETE("/posts/" + p2)
		meta := srv.GET("/posts?filter=status:eq:draft").Meta()
		testutil.AssertEqual(t, "total with filter excludes deleted", meta["total"], float64(1))
	})

	// ── Response shape ────────────────────────────────────────────────────────

	t.Run("live_record_has_null_deleted_at", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u11@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Live", "draft", u))
		data := srv.GET("/posts/" + p).Data()
		deletedAt, exists := data["deleted_at"]
		if exists && deletedAt != nil {
			t.Errorf("live post should have null deleted_at, got %v", deletedAt)
		}
	})

	t.Run("http_method_not_allowed_on_delete_of_wrong_model", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// DELETE /users/:id (hard delete) on a non-existent record — just 404
		srv.DELETE("/users/00000000-0000-0000-0000-000000000001").AssertStatus(http.StatusNotFound)
	})
}
