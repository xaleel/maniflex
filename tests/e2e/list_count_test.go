package e2e

// A list response's meta.total comes from its own COUNT query, separate from the
// data query. It used to be COUNT(DISTINCT <table>.id): every join a list query can
// carry is 1:1 — a nested filter/sort joins a BelongsTo relation on its primary key
// (any other kind is rejected at parse time) and the SQLite FTS join matches one
// shadow row per base row on rowid — so no join fans the base table out and the
// DISTINCT only bought a sort/hash over the whole filtered set. It is now a plain
// COUNT(*) (PERF-1).
//
// These pin the number COUNT(*) has to keep producing. The existing nested-filter
// and sort-relation suites assert the rows a join returns but never meta.total, so
// the count query under a JOIN had no coverage at all — and a count that over-counts
// is exactly how dropping the DISTINCT would go wrong.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// countSeed builds 3 users and 4 posts. Alice authors TWO of them — one related row
// joined by several base rows, the shape that would fan the count out if the join
// were has-many rather than BelongsTo.
func countSeed(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{})
	alice := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "admin"))
	bob := srv.MustID(srv.CreateUser("Bob", "bob@x.com", "editor"))
	carol := srv.MustID(srv.CreateUser("Carol", "carol@x.com", "viewer"))

	srv.MustID(srv.CreatePost("Alpha Post", "published", alice))
	srv.MustID(srv.CreatePost("Beta Post", "draft", bob))
	srv.MustID(srv.CreatePost("Gamma Post", "archived", alice)) // Alice's second
	srv.MustID(srv.CreatePost("Delta Post", "published", carol))
	return srv
}

func assertTotal(t *testing.T, resp *testutil.Response, want float64) {
	t.Helper()
	resp.AssertStatus(http.StatusOK)
	testutil.AssertEqual(t, "meta.total", resp.Meta()["total"], want)
}

func TestListTotal_UnderJoins(t *testing.T) {
	t.Parallel()

	t.Run("no join baseline", func(t *testing.T) {
		t.Parallel()
		assertTotal(t, countSeed(t).GET("/posts"), 4)
	})

	// The join here is Post -> User on the user's primary key. Two of the matched
	// posts share one author, so a fan-out would report 3+ instead of 2.
	t.Run("nested filter join counts base rows, not joined rows", func(t *testing.T) {
		t.Parallel()
		srv := countSeed(t)
		resp := srv.GET("/posts?filter=user.role:eq:admin")
		assertTotal(t, resp, 2)
		testutil.AssertLen(t, "rows", resp.DataList(), 2) // total agrees with the data query
	})

	t.Run("nested sort join does not change the total", func(t *testing.T) {
		t.Parallel()
		assertTotal(t, countSeed(t).GET("/posts?sort=user.name:asc"), 4)
	})

	// Filter and sort naming the same relation join it once; naming it at all must
	// not disturb the count.
	t.Run("filter and sort joins together", func(t *testing.T) {
		t.Parallel()
		assertTotal(t, countSeed(t).GET("/posts?filter=user.role:eq:admin&sort=user.name:asc"), 2)
	})

	// The count query is built independently of LIMIT/OFFSET — the regression that
	// would hide behind a page of results.
	t.Run("total is the full match count, not the page size", func(t *testing.T) {
		t.Parallel()
		srv := countSeed(t)
		resp := srv.GET("/posts?filter=user.role:eq:admin&limit=1")
		assertTotal(t, resp, 2)
		testutil.AssertLen(t, "page", resp.DataList(), 1)
	})

	t.Run("nested filter matching nothing totals zero", func(t *testing.T) {
		t.Parallel()
		assertTotal(t, countSeed(t).GET("/posts?filter=user.name:eq:Nobody"), 0)
	})
}

// The FTS join is the other join a list query can carry: SQLite joins the FTS5
// shadow table on rowid, one shadow row per base row.
func TestListTotal_UnderSearchJoin(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T) *testutil.Server {
		t.Helper()
		srv := ftsServer(t)
		seedArticle(t, srv, "Widget handbook", "all about widgets", "a")
		seedArticle(t, srv, "Widget repair", "fixing a widget", "b")
		seedArticle(t, srv, "Gadget primer", "nothing to see", "a")
		return srv
	}

	t.Run("search join counts matching base rows", func(t *testing.T) {
		t.Parallel()
		srv := seed(t)
		resp := srv.GET("/search_docs?q=widget")
		assertTotal(t, resp, 2)
		testutil.AssertLen(t, "rows", resp.DataList(), 2)
	})

	t.Run("search combined with a filter", func(t *testing.T) {
		t.Parallel()
		assertTotal(t, seed(t).GET("/search_docs?q=widget&filter=tag:eq:a"), 1)
	})

	t.Run("search total survives paging", func(t *testing.T) {
		t.Parallel()
		resp := seed(t).GET("/search_docs?q=widget&limit=1")
		assertTotal(t, resp, 2)
		testutil.AssertLen(t, "page", resp.DataList(), 1)
	})
}
