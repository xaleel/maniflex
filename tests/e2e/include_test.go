package e2e

import (
	"net/http"
	"testing"

	"maniflex/tests/e2e/testutil"
)

// TestInclude covers the ?include= query parameter for relation population.
func TestInclude(t *testing.T) {
	t.Parallel()

	// seed creates: 2 users, 2 posts each, 2 comments per post.
	seed := func(t *testing.T) (*testutil.Server, string, string, string, string) {
		t.Helper()
		srv := testutil.NewServer(t, testutil.Options{})
		u1 := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "admin"))
		u2 := srv.MustID(srv.CreateUser("Bob", "bob@x.com", "editor"))
		p1 := srv.MustID(srv.CreatePost("Post One", "published", u1))
		p2 := srv.MustID(srv.CreatePost("Post Two", "draft", u2))
		srv.MustID(srv.CreateComment("Comment A", p1, u2))
		srv.MustID(srv.CreateComment("Comment B", p1, u1))
		srv.MustID(srv.CreateComment("Comment C", p2, u1))
		return srv, u1, u2, p1, p2
	}

	// ── BelongsTo ─────────────────────────────────────────────────────────────

	t.Run("include_belongs_to_on_single_read", func(t *testing.T) {
		t.Parallel()
		srv, _, _, p1, _ := seed(t)
		resp := srv.GET("/posts/" + p1 + "?include=user")
		resp.AssertStatus(http.StatusOK)
		data := resp.Data()
		user, ok := data["user"].(map[string]any)
		if !ok {
			t.Fatalf("expected 'user' to be an object, got %T", data["user"])
		}
		testutil.AssertEqual(t, "included user name", testutil.Field(t, user, "name"), "Alice")
	})

	t.Run("include_belongs_to_on_list", func(t *testing.T) {
		t.Parallel()
		srv, u1, u2, _, _ := seed(t)
		items := srv.GET("/posts?include=user").DataList()
		testutil.AssertLen(t, "posts", items, 2)
		for _, item := range items {
			m := item.(map[string]any)
			user, ok := m["user"].(map[string]any)
			if !ok {
				t.Errorf("post missing embedded 'user' object: %v", m)
				continue
			}
			// Verify it's the right author
			uid := testutil.Field(t, m, "user_id")
			authorName := testutil.Field(t, user, "name")
			if uid == u1 && authorName != "Alice" {
				t.Errorf("post by u1 should have Alice, got %s", authorName)
			}
			if uid == u2 && authorName != "Bob" {
				t.Errorf("post by u2 should have Bob, got %s", authorName)
			}
		}
	})

	t.Run("include_belongs_to_writeonly_field_excluded_from_nested", func(t *testing.T) {
		t.Parallel()
		srv, _, _, p1, _ := seed(t)
		data := srv.GET("/posts/" + p1 + "?include=user").Data()
		user := data["user"].(map[string]any)
		if _, hasPassword := user["password"]; hasPassword {
			t.Error("writeonly password must not appear in included user object")
		}
	})

	// ── HasMany ───────────────────────────────────────────────────────────────

	t.Run("include_has_many_on_single_read", func(t *testing.T) {
		t.Parallel()
		srv, _, _, p1, _ := seed(t)
		resp := srv.GET("/posts/" + p1 + "?include=comments")
		resp.AssertStatus(http.StatusOK)
		data := resp.Data()
		comments, ok := data["comments"].([]any)
		if !ok {
			t.Fatalf("expected 'comments' to be an array, got %T", data["comments"])
		}
		testutil.AssertLen(t, "comments on p1", comments, 2)
	})

	t.Run("include_has_many_on_list", func(t *testing.T) {
		t.Parallel()
		srv, _, _, _, _ := seed(t)
		items := srv.GET("/posts?include=comments").DataList()
		testutil.AssertLen(t, "posts", items, 2)
		// p1 has 2 comments, p2 has 1 comment
		for _, item := range items {
			m := item.(map[string]any)
			comments, ok := m["comments"].([]any)
			if !ok {
				t.Errorf("post missing embedded 'comments' array: %v", m)
			}
			title := testutil.Field(t, m, "title")
			if title == "Post One" && len(comments) != 2 {
				t.Errorf("Post One should have 2 comments, got %d", len(comments))
			}
			if title == "Post Two" && len(comments) != 1 {
				t.Errorf("Post Two should have 1 comment, got %d", len(comments))
			}
		}
	})

	t.Run("include_has_many_empty_returns_empty_array_not_null", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "u@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Lonely Post", "draft", u))
		data := srv.GET("/posts/" + p + "?include=comments").Data()
		comments, ok := data["comments"].([]any)
		if !ok {
			t.Fatalf("expected empty array for comments, got %T", data["comments"])
		}
		testutil.AssertLen(t, "no comments", comments, 0)
	})

	// ── Multiple includes ─────────────────────────────────────────────────────

	t.Run("multiple_includes_in_one_request", func(t *testing.T) {
		t.Parallel()
		srv, _, _, p1, _ := seed(t)
		data := srv.GET("/posts/" + p1 + "?include=user,comments").Data()
		if _, ok := data["user"].(map[string]any); !ok {
			t.Error("expected embedded 'user' object")
		}
		if _, ok := data["comments"].([]any); !ok {
			t.Error("expected embedded 'comments' array")
		}
	})

	t.Run("has_many_user_includes_posts", func(t *testing.T) {
		t.Parallel()
		srv, u1, _, _, _ := seed(t)
		data := srv.GET("/users/" + u1 + "?include=posts").Data()
		posts, ok := data["posts"].([]any)
		if !ok {
			t.Fatalf("expected 'posts' array, got %T", data["posts"])
		}
		testutil.AssertLen(t, "alice posts", posts, 1)
	})

	// ── Include + filter interaction ──────────────────────────────────────────

	t.Run("include_with_filter_returns_filtered_included_records", func(t *testing.T) {
		t.Parallel()
		srv, _, _, _, _ := seed(t)
		// Only published posts, include their author
		items := srv.GET("/posts?filter=status:eq:published&include=user").DataList()
		testutil.AssertLen(t, "published with user", items, 1)
		m := items[0].(map[string]any)
		user, ok := m["user"].(map[string]any)
		if !ok {
			t.Error("expected user embedded in published post")
		}
		testutil.AssertEqual(t, "author", testutil.Field(t, user, "name"), "Alice")
	})

	// ── Error cases ───────────────────────────────────────────────────────────

	t.Run("include_nonexistent_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv, _, _, p1, _ := seed(t)
		srv.GET("/posts/" + p1 + "?include=category").AssertStatus(http.StatusBadRequest)
	})

	t.Run("include_on_model_with_no_relations_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.POST("/tags", map[string]any{"name": "Go", "color": "blue"}))
		srv.GET("/tags?include=anything").AssertStatus(http.StatusBadRequest)
	})
}
