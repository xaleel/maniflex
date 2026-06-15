package e2e

// 4.3 — sort on BelongsTo relation fields. ?sort=user.name:asc adds a LEFT JOIN
// on the relation and orders by the related table's column, even when no filter
// references the relation. The related field must be marked sortable; unknown
// relations/fields and non-BelongsTo relations return 400.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// sortRelSeed creates three posts whose authors' names order oppositely to the
// post titles, so a correct relation sort is distinguishable from a title sort.
//
//	author Anna  → title "Charlie"
//	author Mike  → title "Bravo"
//	author Zoe   → title "Alpha"
func sortRelSeed(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{})
	anna := srv.MustID(srv.CreateUser("Anna", "anna@sort.com", "admin"))
	mike := srv.MustID(srv.CreateUser("Mike", "mike@sort.com", "editor"))
	zoe := srv.MustID(srv.CreateUser("Zoe", "zoe@sort.com", "viewer"))
	srv.MustID(srv.CreatePost("Charlie", "published", anna))
	srv.MustID(srv.CreatePost("Bravo", "published", mike))
	srv.MustID(srv.CreatePost("Alpha", "published", zoe))
	return srv
}

func postTitles(t *testing.T, items []any) []string {
	t.Helper()
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = testutil.Field(t, it.(map[string]any), "title")
	}
	return out
}

func TestSortOnRelation(t *testing.T) {
	t.Parallel()

	t.Run("sort_asc_by_belongs_to_field", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		items := srv.GET("/posts?sort=user.name:asc").DataList()
		testutil.AssertLen(t, "posts", items, 3)
		// Anna < Mike < Zoe → titles Charlie, Bravo, Alpha.
		got := postTitles(t, items)
		testutil.AssertEqual(t, "first", got[0], "Charlie")
		testutil.AssertEqual(t, "second", got[1], "Bravo")
		testutil.AssertEqual(t, "third", got[2], "Alpha")
	})

	t.Run("sort_desc_by_belongs_to_field", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		items := srv.GET("/posts?sort=user.name:desc").DataList()
		got := postTitles(t, items)
		testutil.AssertEqual(t, "first", got[0], "Alpha")   // Zoe
		testutil.AssertEqual(t, "third", got[2], "Charlie") // Anna
	})

	t.Run("relation_sort_works_without_any_filter", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		// No ?filter= references the relation — the JOIN must still be added.
		got := postTitles(t, srv.GET("/posts?sort=user.role:asc").DataList())
		// roles: admin (Anna/Charlie) < editor (Mike/Bravo) < viewer (Zoe/Alpha)
		testutil.AssertEqual(t, "first", got[0], "Charlie")
		testutil.AssertEqual(t, "third", got[2], "Alpha")
	})

	t.Run("relation_sort_combined_with_relation_filter_shares_join", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		// Filter and sort reference the same relation; the builder must emit a
		// single JOIN alias (no duplicate-alias SQL error).
		items := srv.GET("/posts?filter=user.role:neq:admin&sort=user.name:asc").DataList()
		testutil.AssertLen(t, "non-admin posts", items, 2)
		got := postTitles(t, items)
		testutil.AssertEqual(t, "first", got[0], "Bravo")  // Mike
		testutil.AssertEqual(t, "second", got[1], "Alpha") // Zoe
	})

	t.Run("relation_sort_combined_with_flat_filter", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		got := postTitles(t, srv.GET("/posts?filter=status:eq:published&sort=user.name:desc").DataList())
		testutil.AssertEqual(t, "first", got[0], "Alpha")   // Zoe
		testutil.AssertEqual(t, "third", got[2], "Charlie") // Anna
	})

	t.Run("sort_on_non_sortable_relation_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		// email is filterable but not sortable on User.
		srv.GET("/posts?sort=user.email:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_unknown_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		srv.GET("/posts?sort=category.name:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_unknown_field_of_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		srv.GET("/posts?sort=user.nonexistent:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_has_many_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := sortRelSeed(t)
		// comments is a HasMany on Post — nested sorts are BelongsTo-only.
		srv.GET("/posts?sort=comments.body:asc").AssertStatus(http.StatusBadRequest)
	})
}
