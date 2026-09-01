package e2e

// GAP-10. An offset list runs a COUNT over the whole filtered set on every page
// to fill meta.total. ?count=false declines it: no COUNT, no total, no pages —
// has_more in their place, so a client that only pages forward can still render
// a next-page control.
//
// The COUNT's absence is pinned in db/sqlcore/count_skip_test.go, where an
// adapter with no database proves no statement was issued. These pin what the
// client sees.
//
//	go test ./tests/e2e/... -run TestCount

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// suppressionSeed builds four posts under three users — the same shape
// list_count_test.go counts, so the two suites describe one dataset.
func suppressionSeed(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{})
	alice := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "admin"))
	bob := srv.MustID(srv.CreateUser("Bob", "bob@x.com", "editor"))

	srv.MustID(srv.CreatePost("Alpha Post", "published", alice))
	srv.MustID(srv.CreatePost("Beta Post", "draft", bob))
	srv.MustID(srv.CreatePost("Gamma Post", "archived", alice))
	srv.MustID(srv.CreatePost("Delta Post", "published", bob))
	return srv
}

func sameTitles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCountSuppression_MetaShape(t *testing.T) {
	t.Parallel()

	resp := suppressionSeed(t).GET("/posts?count=false")
	resp.AssertStatus(http.StatusOK)

	meta := resp.Meta()
	for _, absent := range []string{"total", "pages"} {
		if _, ok := meta[absent]; ok {
			t.Errorf("an uncounted page carries %q — nothing counted it: %v", absent, meta)
		}
	}
	testutil.AssertEqual(t, "meta.page", meta["page"], float64(1))
	testutil.AssertEqual(t, "meta.limit", meta["limit"], float64(20))
	testutil.AssertEqual(t, "meta.has_more", meta["has_more"], false)
	testutil.AssertLen(t, "rows", resp.DataList(), 4)
}

// Declining the count must change what the meta says and nothing else. A page
// that quietly returned different rows — the over-fetched probe row among them —
// would be the expensive kind of wrong.
func TestCountSuppression_ReturnsTheSameRowsAsACountedPage(t *testing.T) {
	t.Parallel()

	srv := suppressionSeed(t)
	for _, query := range []string{
		"/posts?limit=3",
		"/posts?limit=3&page=2",
		"/posts?filter=status:eq:published",
		"/posts?filter=user.role:eq:admin&sort=title:asc",
	} {
		t.Run(query, func(t *testing.T) {
			counted := srv.GET(query)
			counted.AssertStatus(http.StatusOK)
			uncounted := srv.GET(query + "&count=false")
			uncounted.AssertStatus(http.StatusOK)

			if got, want := postTitles(t, uncounted.DataList()), postTitles(t, counted.DataList()); !sameTitles(got, want) {
				t.Errorf("?count=false changed the page: got %v, want %v", got, want)
			}
		})
	}
}

// has_more is what replaces the total, so it has to be right at the boundary —
// including the page that is exactly full and has nothing after it, where a
// naive "a full page means more" would lie.
func TestCountSuppression_HasMore(t *testing.T) {
	t.Parallel()

	srv := suppressionSeed(t) // 4 posts

	cases := []struct {
		query string
		rows  int
		more  bool
	}{
		{query: "/posts?count=false&limit=2", rows: 2, more: true},
		{query: "/posts?count=false&limit=2&page=2", rows: 2, more: false},
		{query: "/posts?count=false&limit=4", rows: 4, more: false},
		{query: "/posts?count=false&limit=3&page=2", rows: 1, more: false},
		{query: "/posts?count=false&limit=2&page=9", rows: 0, more: false},
		{query: "/posts?count=false&filter=status:eq:published&limit=1", rows: 1, more: true},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp := srv.GET(tc.query)
			resp.AssertStatus(http.StatusOK)
			testutil.AssertLen(t, "rows", resp.DataList(), tc.rows)
			testutil.AssertEqual(t, "meta.has_more", resp.Meta()["has_more"], tc.more)
		})
	}
}

func TestCountSuppression_CountTrueIsTheOrdinaryList(t *testing.T) {
	t.Parallel()

	resp := suppressionSeed(t).GET("/posts?count=true")
	resp.AssertStatus(http.StatusOK)
	testutil.AssertEqual(t, "meta.total", resp.Meta()["total"], float64(4))
	if _, ok := resp.Meta()["has_more"]; ok {
		t.Errorf("a counted page carries has_more: %v", resp.Meta())
	}
}

func TestCountSuppression_RejectsAValueItCannotRead(t *testing.T) {
	t.Parallel()

	srv := suppressionSeed(t)
	for _, raw := range []string{"", "yes", "maybe"} {
		resp := srv.GET("/posts?count=" + raw)
		resp.AssertStatus(http.StatusBadRequest)
		testutil.AssertEqual(t, "error.code", resp.ErrorCode(), "INVALID_QUERY")
	}
}

// ?count= and ?cursor= overlap. Keyset pagination has never run the count, so
// ?count=false agrees with it and is allowed; ?count=true asks for a total the
// cursor envelope has nowhere to put, and answering with the cursor shape
// anyway would look to the client like a total that happened to be missing.
func TestCountSuppression_WithCursorPagination(t *testing.T) {
	t.Parallel()

	srv := cursorServer(t)
	seedEvents(t, srv, 3)

	t.Run("count=false keeps the cursor shape", func(t *testing.T) {
		resp := srv.GET("/cursor_events?cursor=&limit=2&count=false")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "rows", resp.DataList(), 2)
		meta := resp.Meta()
		testutil.AssertEqual(t, "meta.has_more", meta["has_more"], true)
		testutil.AssertNotEmpty(t, "next_cursor", meta["next_cursor"].(string))
		if _, ok := meta["total"]; ok {
			t.Errorf("cursor meta carries a total: %v", meta)
		}
	})

	t.Run("count=true is refused", func(t *testing.T) {
		resp := srv.GET("/cursor_events?cursor=&limit=2&count=true")
		resp.AssertStatus(http.StatusBadRequest)
		testutil.AssertEqual(t, "error.code", resp.ErrorCode(), "INVALID_QUERY")
	})
}

// An export never carried a total — it streams rows and writes no meta block —
// but it ran the COUNT for one anyway, over the largest filtered set the
// framework serves. The parameter is accepted on the route and changes nothing,
// because the export declines the count either way.
func TestCountSuppression_ExportIsUnaffected(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, 0)
	seedRows(t, srv, 3)
	with := srv.GETRaw("/exportable_rows/export?format=csv&count=false")
	with.AssertStatus(http.StatusOK)
	without := srv.GETRaw("/exportable_rows/export?format=csv")
	without.AssertStatus(http.StatusOK)

	if string(with.Body) != string(without.Body) {
		t.Errorf("?count=false changed the export: got %q, want %q", with.Body, without.Body)
	}
}
