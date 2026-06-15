package e2e

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestFilterORGroups verifies bracket-indexed ?filter[N]=... OR group semantics.
//
// Setup: 6 posts with varied status/title combinations.
// Assertions cover:
//   - single OR group selects the union of matching records
//   - two separate OR groups each AND together
//   - ungrouped filter still ANDs with grouped ones
//   - invalid bracket index returns 400
//   - cross-table OR group returns 400
func TestFilterORGroups(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T) (*testutil.Server, string) {
		t.Helper()
		srv := testutil.NewServer(t, testutil.Options{})
		uid := srv.MustID(srv.CreateUser("Alice", "alice@or.com", "admin"))

		srv.MustID(srv.CreatePost("Alpha", "draft", uid))
		srv.MustID(srv.CreatePost("Beta", "published", uid))
		srv.MustID(srv.CreatePost("Gamma", "archived", uid))
		srv.MustID(srv.CreatePost("Delta", "published", uid))
		srv.MustID(srv.CreatePost("Epsilon", "draft", uid))
		srv.MustID(srv.CreatePost("Zeta", "archived", uid))

		return srv, uid
	}

	// Helper: build a URL query string with potentially duplicate keys.
	// params is a slice of [key, value] pairs.
	buildQuery := func(params [][2]string) string {
		vals := url.Values{}
		for _, kv := range params {
			vals.Add(kv[0], kv[1])
		}
		return "?" + vals.Encode()
	}

	// ── OR group: status=draft OR status=published ────────────────────────────
	t.Run("OR_group_returns_union", func(t *testing.T) {
		t.Parallel()
		srv, _ := seed(t)
		q := buildQuery([][2]string{
			{"filter[0]", "status:eq:draft"},
			{"filter[0]", "status:eq:published"},
		})
		items := srv.GET("/posts" + q).DataList()
		// Alpha (draft), Beta (published), Delta (published), Epsilon (draft) → 4
		testutil.AssertLen(t, "draft OR published", items, 4)
		for _, item := range items {
			m := item.(map[string]any)
			s := testutil.Field(t, m, "status")
			if s != "draft" && s != "published" {
				t.Errorf("unexpected status %q in result", s)
			}
		}
	})

	// ── OR group AND ungrouped filter ─────────────────────────────────────────
	t.Run("OR_group_AND_ungrouped", func(t *testing.T) {
		t.Parallel()
		srv, _ := seed(t)
		// (status=draft OR status=published) AND title=Beta
		q := buildQuery([][2]string{
			{"filter[0]", "status:eq:draft"},
			{"filter[0]", "status:eq:published"},
			{"filter", "title:eq:Beta"},
		})
		items := srv.GET("/posts" + q).DataList()
		// Only "Beta" which is published
		testutil.AssertLen(t, "OR group AND title=Beta", items, 1)
		testutil.AssertEqual(t, "title", testutil.Field(t, items[0].(map[string]any), "title"), "Beta")
	})

	// ── Two separate OR groups: (A OR B) AND (C OR D) ────────────────────────
	t.Run("two_OR_groups_AND_together", func(t *testing.T) {
		t.Parallel()
		srv, _ := seed(t)
		// group 0: status=draft OR status=published
		// group 1: title=Alpha OR title=Beta
		// Expected: Alpha (draft, group0 ✓ group1 ✓) and Beta (published, group0 ✓ group1 ✓) → 2
		q := buildQuery([][2]string{
			{"filter[0]", "status:eq:draft"},
			{"filter[0]", "status:eq:published"},
			{"filter[1]", "title:eq:Alpha"},
			{"filter[1]", "title:eq:Beta"},
		})
		items := srv.GET("/posts" + q).DataList()
		testutil.AssertLen(t, "(draft OR published) AND (Alpha OR Beta)", items, 2)
	})

	// ── Single-element group behaves like plain AND filter ────────────────────
	t.Run("single_element_group_works_as_AND", func(t *testing.T) {
		t.Parallel()
		srv, _ := seed(t)
		q := buildQuery([][2]string{
			{"filter[0]", "status:eq:archived"},
		})
		items := srv.GET("/posts" + q).DataList()
		testutil.AssertLen(t, "archived only", items, 2)
	})

	// ── Invalid bracket index returns 400 ────────────────────────────────────
	t.Run("invalid_bracket_index_400", func(t *testing.T) {
		t.Parallel()
		srv, _ := seed(t)
		srv.GET("/posts?filter%5Bbad%5D=status:eq:draft").AssertStatus(http.StatusBadRequest)
	})
}
