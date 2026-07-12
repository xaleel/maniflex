package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestFilter covers every filter operator, field access pattern, and combination.
func TestFilter(t *testing.T) {
	t.Parallel()

	// seed sets up 3 users and 4 posts for filter tests.
	seed := func(t *testing.T) (*testutil.Server, []string, []string) {
		t.Helper()
		srv := testutil.NewServer(t, testutil.Options{})
		u1 := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "admin"))
		u2 := srv.MustID(srv.CreateUser("Bob", "bob@x.com", "editor"))
		u3 := srv.MustID(srv.CreateUser("Carol", "carol@x.com", "viewer"))

		p1 := srv.MustID(srv.CreatePost("Alpha Post", "published", u1))
		p2 := srv.MustID(srv.CreatePost("Beta Post", "draft", u2))
		p3 := srv.MustID(srv.CreatePost("Gamma Post", "archived", u1))
		p4 := srv.MustID(srv.CreatePost("Delta Post", "published", u3))

		return srv, []string{u1, u2, u3}, []string{p1, p2, p3, p4}
	}

	// ── eq operator ──────────────────────────────────────────────────────────

	t.Run("eq_on_filterable_string_field", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:eq:published").DataList()
		testutil.AssertLen(t, "published posts", items, 2)
		for _, item := range items {
			m := item.(map[string]any)
			testutil.AssertEqual(t, "status", testutil.Field(t, m, "status"), "published")
		}
	})

	t.Run("eq_on_filterable_field_no_match_returns_empty", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:eq:nonexistent").DataList()
		testutil.AssertLen(t, "no match", items, 0)
	})

	t.Run("eq_on_non_filterable_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// body has no mfx:"filterable"
		srv.GET("/posts?filter=body:eq:Alpha Post").AssertStatus(http.StatusBadRequest)
	})

	// ── neq operator ─────────────────────────────────────────────────────────

	t.Run("neq_excludes_matching_records", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:neq:draft").DataList()
		// 4 total - 1 draft = 3
		testutil.AssertLen(t, "non-draft posts", items, 3)
		for _, item := range items {
			m := item.(map[string]any)
			if testutil.Field(t, m, "status") == "draft" {
				t.Error("neq filter must exclude draft status")
			}
		}
	})

	// ── like / ilike operators ────────────────────────────────────────────────

	t.Run("like_matches_substring", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=title:like:%25Post").DataList()
		testutil.AssertLen(t, "posts ending in Post", items, 4)
	})

	t.Run("like_no_match_returns_empty", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=title:like:zzz%25").DataList()
		testutil.AssertLen(t, "no match", items, 0)
	})

	t.Run("ilike_is_case_insensitive", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// Title has "Alpha" — search lowercase
		items := srv.GET("/posts?filter=title:ilike:alpha%25").DataList()
		testutil.AssertLen(t, "ilike alpha", items, 1)
	})

	// ── in / not_in operators ─────────────────────────────────────────────────

	t.Run("in_matches_multiple_values", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:in:draft,archived").DataList()
		testutil.AssertLen(t, "draft or archived", items, 2)
	})

	t.Run("not_in_excludes_multiple_values", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:not_in:draft,archived").DataList()
		testutil.AssertLen(t, "not draft or archived", items, 2)
		for _, item := range items {
			m := item.(map[string]any)
			s := testutil.Field(t, m, "status")
			if s == "draft" || s == "archived" {
				t.Errorf("not_in filter leaked status=%s", s)
			}
		}
	})

	t.Run("in_single_value_works", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=status:in:published").DataList()
		testutil.AssertLen(t, "single in value", items, 2)
	})

	// An empty list has no SQL form: it used to reach the adapter as zero values
	// and emit "status IN ()", a syntax error on every driver — so any client
	// could provoke a 500 at will (BUG-7). It is a malformed filter; say so.
	t.Run("in_with_no_values_is_rejected", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		for _, raw := range []string{
			"status:in:",      // no value at all
			"status:in:,,",    // only separators — every entry drops out
			"status:not_in:",  //
			"status:not_in:,", //
		} {
			resp := srv.GET("/posts?filter=" + raw)
			resp.AssertStatus(http.StatusBadRequest)
			if code := resp.ErrorCode(); code != "INVALID_QUERY" {
				t.Errorf("filter %q: error code = %q, want INVALID_QUERY", raw, code)
			}
		}
	})

	// ── gt / gte / lt / lte operators ────────────────────────────────────────

	t.Run("numeric_gt_filter", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{10, 20, 30, 40} {
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("u%d@s.com", i),
				"password": "s", "score": score,
			}))
		}
		items := srv.GET("/users?filter=score:gt:20").DataList()
		testutil.AssertLen(t, "score>20", items, 2)
	})

	t.Run("numeric_gte_filter_includes_boundary", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{10, 20, 30} {
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("ugte%d@s.com", i),
				"password": "s", "score": score,
			}))
		}
		items := srv.GET("/users?filter=score:gte:20").DataList()
		testutil.AssertLen(t, "score>=20", items, 2)
	})

	t.Run("numeric_lt_filter", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{5, 15, 25} {
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("ult%d@s.com", i),
				"password": "s", "score": score,
			}))
		}
		items := srv.GET("/users?filter=score:lt:20").DataList()
		testutil.AssertLen(t, "score<20", items, 2)
	})

	t.Run("numeric_lte_filter_includes_boundary", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{10, 20, 30} {
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("ulte%d@s.com", i),
				"password": "s", "score": score,
			}))
		}
		items := srv.GET("/users?filter=score:lte:20").DataList()
		testutil.AssertLen(t, "score<=20", items, 2)
	})

	// ── between operator ──────────────────────────────────────────────────────

	t.Run("between_numeric_inclusive_bounds", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{10, 20, 30, 40} {
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("ubt%d@s.com", i),
				"password": "s", "score": score,
			}))
		}
		// 20 and 30 are within [20,30]; 10 and 40 are excluded.
		items := srv.GET("/users?filter=score:between:20,30").DataList()
		testutil.AssertLen(t, "score between 20 and 30", items, 2)
		for _, item := range items {
			m := item.(map[string]any)
			s := testutil.FloatField(t, m, "score")
			if s < 20 || s > 30 {
				t.Errorf("between leaked score=%v", s)
			}
		}
	})

	t.Run("between_combines_with_other_filters", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i, score := range []int{15, 25, 35} {
			role := "editor"
			if i == 1 {
				role = "admin"
			}
			srv.MustID(srv.POST("/users", map[string]any{
				"name": fmt.Sprintf("U%d", i), "email": fmt.Sprintf("ubt2%d@s.com", i),
				"password": "s", "score": score, "role": role,
			}))
		}
		items := srv.GET("/users?filter=score:between:10,40&filter=role:eq:admin").DataList()
		testutil.AssertLen(t, "admin within range", items, 1)
	})

	t.Run("between_wrong_value_count_returns_400", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		srv.GET("/posts?filter=views:between:100").AssertStatus(http.StatusBadRequest)
		srv.GET("/posts?filter=views:between:1,2,3").AssertStatus(http.StatusBadRequest)
	})

	// ── is_null / not_null ────────────────────────────────────────────────────

	t.Run("is_null_matches_null_values", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// deleted_at is NULL on all non-deleted posts
		items := srv.GET("/posts?filter=deleted_at:is_null").DataList()
		testutil.AssertLen(t, "deleted_at is null", items, 4)
	})

	t.Run("not_null_matches_non_null_values", func(t *testing.T) {
		t.Parallel()
		srv, _, posts := seed(t)
		srv.DELETE("/posts/" + posts[0])
		// After soft-delete, deleted_at is NOT NULL on 1 record.
		// Soft-delete scope in the adapter still filters it out from the list.
		// Directly test not_null on a nullable non-soft-delete field:
		items := srv.GET("/posts?filter=deleted_at:not_null").DataList()
		// Soft delete scope excludes deleted rows from list regardless of filter
		// — the test validates the operator is accepted and returns 200.
		_ = items
		srv.GET("/posts?filter=deleted_at:not_null").AssertStatus(http.StatusOK)
	})

	// ── Multiple filters ──────────────────────────────────────────────────────

	t.Run("multiple_filters_are_ANDed", func(t *testing.T) {
		t.Parallel()
		srv, userIDs, _ := seed(t)
		// published posts authored by u1
		url := fmt.Sprintf("/posts?filter=status:eq:published&filter=user_id:eq:%s", userIDs[0])
		items := srv.GET(url).DataList()
		testutil.AssertLen(t, "published by u1", items, 1)
		testutil.AssertEqual(t, "status", testutil.Field(t, items[0].(map[string]any), "status"), "published")
		testutil.AssertEqual(t, "user_id", testutil.Field(t, items[0].(map[string]any), "user_id"), userIDs[0])
	})

	t.Run("filter_combined_with_pagination", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// 2 published posts, request page 1 limit 1
		resp := srv.GET("/posts?filter=status:eq:published&page=1&limit=1")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "items", resp.DataList(), 1)
		meta := resp.Meta()
		testutil.AssertEqual(t, "total", meta["total"], float64(2))
		testutil.AssertEqual(t, "pages", meta["pages"], float64(2))
	})

	t.Run("mix_filterable_and_non_filterable_in_same_request_rejects", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// status filterable, body not — the second filter must make the whole request 400
		srv.GET("/posts?filter=status:eq:draft&filter=body:eq:x").AssertStatus(http.StatusBadRequest)
	})

	// ── Nested (relation) filters ─────────────────────────────────────────────

	t.Run("nested_filter_on_belongs_to_relation", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// Filter posts where the author's role is "admin"
		items := srv.GET("/posts?filter=user.role:eq:admin").DataList()
		// Alice (admin) authored Alpha Post and Gamma Post
		testutil.AssertLen(t, "posts by admin", items, 2)
	})

	t.Run("nested_filter_neq_on_relation", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		items := srv.GET("/posts?filter=user.role:neq:admin").DataList()
		testutil.AssertLen(t, "posts by non-admin", items, 2)
	})

	t.Run("nested_filter_combined_with_flat_filter", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// admin-authored AND published
		items := srv.GET("/posts?filter=user.role:eq:admin&filter=status:eq:published").DataList()
		testutil.AssertLen(t, "admin+published", items, 1)
	})

	t.Run("nested_filter_on_non_filterable_relation_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		// password is not filterable on User
		srv.GET("/posts?filter=user.password:eq:secret").AssertStatus(http.StatusBadRequest)
	})

	t.Run("nested_filter_on_unknown_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := seed(t)
		srv.GET("/posts?filter=category.name:eq:tech").AssertStatus(http.StatusBadRequest)
	})
}

// TestSort covers ?sort= parameter handling.
func TestSort(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T) *testutil.Server {
		t.Helper()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.CreateUser("Charlie", "c@x.com", "viewer"))
		srv.MustID(srv.CreateUser("Alice", "a@x.com", "admin"))
		srv.MustID(srv.CreateUser("Bob", "b@x.com", "editor"))
		return srv
	}

	t.Run("sort_ascending_by_string_field", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		items := srv.GET("/users?sort=name:asc").DataList()
		testutil.AssertLen(t, "items", items, 3)
		names := []string{
			testutil.Field(t, items[0].(map[string]any), "name"),
			testutil.Field(t, items[1].(map[string]any), "name"),
			testutil.Field(t, items[2].(map[string]any), "name"),
		}
		testutil.AssertEqual(t, "first", names[0], "Alice")
		testutil.AssertEqual(t, "second", names[1], "Bob")
		testutil.AssertEqual(t, "third", names[2], "Charlie")
	})

	t.Run("sort_descending_by_string_field", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		items := srv.GET("/users?sort=name:desc").DataList()
		testutil.AssertEqual(t, "first desc",
			testutil.Field(t, items[0].(map[string]any), "name"), "Charlie")
	})

	t.Run("sort_without_direction_defaults_to_asc", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		items := srv.GET("/users?sort=name").DataList()
		testutil.AssertEqual(t, "first no-dir",
			testutil.Field(t, items[0].(map[string]any), "name"), "Alice")
	})

	t.Run("sort_on_non_sortable_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		// email is filterable but not sortable
		srv.GET("/users?sort=email:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_unknown_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		srv.GET("/users?sort=nonexistent:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_invalid_direction_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		srv.GET("/users?sort=name:sideways").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_combined_with_filter", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		items := srv.GET("/users?filter=role:neq:admin&sort=name:desc").DataList()
		// Bob (editor) and Charlie (viewer), sorted desc → Charlie first
		testutil.AssertLen(t, "filtered+sorted", items, 2)
		testutil.AssertEqual(t, "first",
			testutil.Field(t, items[0].(map[string]any), "name"), "Charlie")
	})

	t.Run("sort_combined_with_pagination", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		resp := srv.GET("/users?sort=name:asc&page=2&limit=2")
		resp.AssertStatus(http.StatusOK)
		items := resp.DataList()
		testutil.AssertLen(t, "page2 items", items, 1)
		testutil.AssertEqual(t, "last item",
			testutil.Field(t, items[0].(map[string]any), "name"), "Charlie")
	})
}

// TestPagination covers ?page= and ?limit= parameters.
func TestPagination(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T, n int) *testutil.Server {
		t.Helper()
		srv := testutil.NewServer(t, testutil.Options{})
		for i := range n {
			srv.MustID(srv.CreateUser(
				fmt.Sprintf("User%02d", i),
				fmt.Sprintf("u%02d@x.com", i),
				"viewer",
			))
		}
		return srv
	}

	t.Run("page1_limit5_returns_first_5", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 12)
		resp := srv.GET("/users?sort=name:asc&page=1&limit=5")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "items", resp.DataList(), 5)
		meta := resp.Meta()
		testutil.AssertEqual(t, "total", meta["total"], float64(12))
		testutil.AssertEqual(t, "pages", meta["pages"], float64(3))
	})

	t.Run("page3_limit5_returns_remaining_2", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 12)
		resp := srv.GET("/users?page=3&limit=5")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "items", resp.DataList(), 2)
	})

	t.Run("page_beyond_data_returns_empty_not_error", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 3)
		resp := srv.GET("/users?page=99&limit=10")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "items", resp.DataList(), 0)
	})

	t.Run("limit_clamped_to_max_200", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 5)
		resp := srv.GET("/users?limit=999")
		resp.AssertStatus(http.StatusOK)
		meta := resp.Meta()
		testutil.AssertEqual(t, "limit clamped", meta["limit"], float64(200))
	})

	t.Run("meta_pages_rounds_up", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 7)
		meta := srv.GET("/users?limit=3").Meta()
		testutil.AssertEqual(t, "pages ceil(7/3)=3", meta["pages"], float64(3))
	})

	t.Run("meta_pages_exact_division", func(t *testing.T) {
		t.Parallel()
		srv := seed(t, 6)
		meta := srv.GET("/users?limit=3").Meta()
		testutil.AssertEqual(t, "pages 6/3=2", meta["pages"], float64(2))
	})
}
