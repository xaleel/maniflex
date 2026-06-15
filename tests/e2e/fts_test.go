package e2e

// End-to-end coverage for per-model full-text search — roadmap 4.9.
//
//	go test ./tests/e2e/... -run TestFullTextSearch
//
// Exercises the ?q= search parameter over mfx:"searchable" fields: native FTS
// ranking/stemming, relevance ordering, combination with ?filter=, soft-delete
// exclusion, index maintenance on update/delete, and the rejection paths.

import (
	"net/http"
	"slices"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// SearchDoc is searchable on title + body, soft-deletable (so soft-deleted rows
// can be checked against the index), and cursor-enabled (so the ?q=+?cursor=
// rejection path has a model that supports both).
type SearchDoc struct {
	maniflex.BaseModel `mfx:"cursor_field:created_at"`
	maniflex.WithDeletedAt
	Title string `json:"title" db:"title" mfx:"required,searchable"`
	Body  string `json:"body"  db:"body"  mfx:"searchable"`
	Tag   string `json:"tag"   db:"tag"   mfx:"filterable"`
}

// PlainDoc has no searchable fields — probes the "?q= not enabled" rejection.
type PlainDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name"`
}

func ftsServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: []any{SearchDoc{}, PlainDoc{}}})
}

func seedArticle(t *testing.T, srv *testutil.Server, title, body, tag string) string {
	t.Helper()
	resp := srv.POST("/search_docs", map[string]any{"title": title, "body": body, "tag": tag})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// titles extracts the title values from a list response, in order.
func titles(t *testing.T, resp *testutil.Response) []string {
	t.Helper()
	var out []string
	for _, it := range resp.DataList() {
		m := it.(map[string]any)
		out = append(out, m["title"].(string))
	}
	return out
}

func TestFullTextSearch(t *testing.T) {
	t.Parallel()

	t.Run("matches_searchable_fields_and_excludes_non_matches", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		seedArticle(t, srv, "Postgres tuning guide", "indexes and vacuum", "db")
		seedArticle(t, srv, "Cooking pasta", "boil water and add salt", "food")
		seedArticle(t, srv, "Database basics", "what is postgres anyway", "db")

		resp := srv.GET("/search_docs?q=postgres")
		resp.AssertStatus(http.StatusOK)
		got := titles(t, resp)
		if len(got) != 2 {
			t.Fatalf("q=postgres returned %d rows, want 2: %v", len(got), got)
		}
		if !slices.Contains(got, "Postgres tuning guide") || !slices.Contains(got, "Database basics") {
			t.Errorf("q=postgres titles = %v, want the two postgres articles", got)
		}
		if slices.Contains(got, "Cooking pasta") {
			t.Errorf("q=postgres should not match the pasta article: %v", got)
		}
		// Offset-mode meta: total reflects matched rows, not the whole table.
		if total, _ := resp.Meta()["total"].(float64); int(total) != 2 {
			t.Errorf("meta.total = %v, want 2", resp.Meta()["total"])
		}
	})

	t.Run("stems_tokens", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		seedArticle(t, srv, "Morning routine", "running and jumping every day", "fit")

		// porter stemming: "run" matches "running".
		resp := srv.GET("/search_docs?q=run")
		resp.AssertStatus(http.StatusOK)
		if got := titles(t, resp); len(got) != 1 || got[0] != "Morning routine" {
			t.Fatalf("q=run (stemmed) = %v, want [Morning routine]", got)
		}
	})

	t.Run("orders_by_relevance", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		// Two matches; the denser document should rank first under bm25.
		seedArticle(t, srv, "Sparse", "alpha beta gamma delta", "x")
		seedArticle(t, srv, "Dense", "alpha alpha alpha alpha", "x")

		resp := srv.GET("/search_docs?q=alpha")
		resp.AssertStatus(http.StatusOK)
		got := titles(t, resp)
		if len(got) != 2 {
			t.Fatalf("q=alpha returned %d rows, want 2: %v", len(got), got)
		}
		if got[0] != "Dense" {
			t.Errorf("relevance order = %v, want the denser doc first", got)
		}
	})

	t.Run("combines_with_filter", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		seedArticle(t, srv, "Redis intro", "fast cache store", "db")
		seedArticle(t, srv, "Redis recipes", "fast cache store", "food")

		resp := srv.GET("/search_docs?q=cache&filter=tag:eq:db")
		resp.AssertStatus(http.StatusOK)
		if got := titles(t, resp); len(got) != 1 || got[0] != "Redis intro" {
			t.Fatalf("q=cache&filter=tag:eq:db = %v, want [Redis intro]", got)
		}
	})

	t.Run("excludes_soft_deleted_rows", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		id := seedArticle(t, srv, "Ephemeral note", "transient content keyword", "x")

		srv.GET("/search_docs?q=keyword").AssertStatus(http.StatusOK)

		srv.DELETE("/search_docs/" + id).AssertStatus(http.StatusNoContent)

		resp := srv.GET("/search_docs?q=keyword")
		resp.AssertStatus(http.StatusOK)
		if got := resp.DataList(); len(got) != 0 {
			t.Fatalf("soft-deleted row still searchable: %v", got)
		}
	})

	t.Run("reindexes_on_update", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		id := seedArticle(t, srv, "Mutable", "originalword here", "x")

		// The old token no longer matches once the body is replaced.
		srv.PATCH("/search_docs/"+id, map[string]any{"body": "replacedword now"}).AssertStatus(http.StatusOK)

		if got := srv.GET("/search_docs?q=originalword").DataList(); len(got) != 0 {
			t.Errorf("stale token still matches after update: %v", titles(t, srv.GET("/search_docs?q=originalword")))
		}
		resp := srv.GET("/search_docs?q=replacedword")
		resp.AssertStatus(http.StatusOK)
		if got := titles(t, resp); len(got) != 1 || got[0] != "Mutable" {
			t.Fatalf("q=replacedword = %v, want [Mutable]", got)
		}
	})

	t.Run("empty_q_is_a_no_op", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		seedArticle(t, srv, "One", "aaa", "x")
		seedArticle(t, srv, "Two", "bbb", "x")

		resp := srv.GET("/search_docs?q=")
		resp.AssertStatus(http.StatusOK)
		if got := resp.DataList(); len(got) != 2 {
			t.Fatalf("empty ?q= returned %d rows, want all 2", len(got))
		}
	})

	t.Run("punctuation_only_q_matches_nothing", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		seedArticle(t, srv, "Real", "real content", "x")

		// Input with no word tokens must not error — it simply matches nothing.
		resp := srv.GET("/search_docs?q=%23%23%23") // "###"
		resp.AssertStatus(http.StatusOK)
		if got := resp.DataList(); len(got) != 0 {
			t.Fatalf("punctuation-only q returned %d rows, want 0", len(got))
		}
	})

	t.Run("rejects_q_on_non_searchable_model", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		resp := srv.GET("/plain_docs?q=hello")
		resp.AssertStatus(http.StatusBadRequest)
		if code := resp.ErrorCode(); code != "INVALID_QUERY" {
			t.Errorf("error code = %q, want INVALID_QUERY", code)
		}
	})

	t.Run("rejects_q_combined_with_cursor", func(t *testing.T) {
		t.Parallel()
		srv := ftsServer(t)
		resp := srv.GET("/search_docs?q=hello&cursor=")
		resp.AssertStatus(http.StatusBadRequest)
		if code := resp.ErrorCode(); code != "INVALID_QUERY" {
			t.Errorf("error code = %q, want INVALID_QUERY", code)
		}
	})
}
