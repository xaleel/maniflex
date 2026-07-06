package e2e

// End-to-end coverage for cross-model search — roadmap 4.10.
//
//	go test ./tests/e2e/... -run TestGlobalSearch
//
// Exercises both layers of the feature: the ctx.Search primitive (driven from a
// custom Action with an explicit, app-authorised model list) and the built-in
// GET /search endpoint (Server.EnableGlobalSearch over ModelConfig.GlobalSearchable
// models) — merge/relevance ordering, the PerModelLimit fairness cap with
// backfill, ?models= subsetting, soft-delete exclusion, the rejection paths, the
// OpSearch auth-middleware hook, and the ineffective-registration warning.

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// SearchArticle is searchable on title + body and soft-deletable, opted into the
// built-in /search endpoint via GlobalSearchable.
type SearchArticle struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title string `json:"title" db:"title" mfx:"required,searchable"`
	Body  string `json:"body"  db:"body"  mfx:"searchable"`
}

// SearchMemo is searchable on text and also GlobalSearchable — the second model
// the /search fan-out merges with SearchArticle.
type SearchMemo struct {
	maniflex.BaseModel
	Text string `json:"text" db:"text" mfx:"required,searchable"`
}

// SearchNote is searchable but NOT GlobalSearchable: it is excluded from the
// built-in endpoint yet still usable through ctx.Search with an explicit list
// (the Action-scoped path).
type SearchNote struct {
	maniflex.BaseModel
	Content string `json:"content" db:"content" mfx:"required,searchable"`
}

func searchModels() []any {
	return []any{
		SearchArticle{}, maniflex.ModelConfig{GlobalSearchable: true},
		SearchMemo{}, maniflex.ModelConfig{GlobalSearchable: true},
		SearchNote{},
	}
}

// scopedSearchAction is a custom endpoint that drives the ctx.Search primitive
// directly: q/models/limit/per_model come from the query string so a single
// action exercises the explicit-model path, the validation error path, and the
// PerModelLimit fairness cap.
func scopedSearchAction() maniflex.ActionConfig {
	return maniflex.ActionConfig{
		Method: "GET",
		Path:   "/scoped-search",
		Handler: func(ctx *maniflex.ServerContext) error {
			var models []string
			if raw := ctx.QueryParam("models"); raw != "" {
				models = strings.Split(raw, ",")
			}
			limit, _ := strconv.Atoi(ctx.QueryParam("limit"))
			perModel, _ := strconv.Atoi(ctx.QueryParam("per_model"))
			res, err := ctx.Search(maniflex.SearchOptions{
				Query:         ctx.QueryParam("q"),
				Models:        models,
				Limit:         limit,
				PerModelLimit: perModel,
			})
			if err != nil {
				ctx.Abort(http.StatusBadRequest, "SEARCH_ERROR", err.Error())
				return nil
			}
			ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: res}
			return nil
		},
	}
}

// searchServer builds a test server with the three search models, the built-in
// /search endpoint enabled, the scoped-search action registered, and any extra
// per-test configuration applied last.
func searchServer(t *testing.T, extra ...func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: searchModels(),
		Middleware: func(s *maniflex.Server) {
			s.EnableGlobalSearch()
			s.Action(scopedSearchAction())
			for _, fn := range extra {
				fn(s)
			}
		},
	})
}

func seedArticleDoc(t *testing.T, srv *testutil.Server, title, body string) string {
	t.Helper()
	resp := srv.POST("/search_articles", map[string]any{"title": title, "body": body})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

func seedMemoDoc(t *testing.T, srv *testutil.Server, text string) string {
	t.Helper()
	resp := srv.POST("/search_memos", map[string]any{"text": text})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// modelsOf returns the "model" field of every hit in a search response.
func modelsOf(resp *testutil.Response) []string {
	var out []string
	for _, it := range resp.DataList() {
		out = append(out, it.(map[string]any)["model"].(string))
	}
	return out
}

func countModel(resp *testutil.Response, model string) int {
	n := 0
	for _, m := range modelsOf(resp) {
		if m == model {
			n++
		}
	}
	return n
}

func TestGlobalSearch(t *testing.T) {
	t.Parallel()

	t.Run("primitive_searches_explicit_non_global_model", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		srv.POST("/search_notes", map[string]any{"content": "kubernetes operator pattern"}).AssertStatus(http.StatusCreated)
		srv.POST("/search_notes", map[string]any{"content": "grocery shopping list"}).AssertStatus(http.StatusCreated)

		// SearchNote is not GlobalSearchable, but ctx.Search with an explicit list
		// (the Action-scoped path) still searches it.
		resp := srv.GET("/scoped-search?q=kubernetes&models=SearchNote")
		resp.AssertStatus(http.StatusOK)
		hits := resp.DataList()
		if len(hits) != 1 {
			t.Fatalf("scoped search returned %d hits, want 1: %s", len(hits), resp.Body)
		}
		hit := hits[0].(map[string]any)
		if hit["model"] != "SearchNote" {
			t.Errorf("hit model = %v, want SearchNote", hit["model"])
		}
		if hit["id"] == "" || hit["snippet"] == "" {
			t.Errorf("hit missing id/snippet: %v", hit)
		}
		if _, ok := hit["score"].(float64); !ok {
			t.Errorf("hit score is not a number: %v", hit["score"])
		}
	})

	t.Run("primitive_rejects_unknown_model", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		resp := srv.GET("/scoped-search?q=anything&models=Bogus")
		resp.AssertStatus(http.StatusBadRequest)
		if code := resp.ErrorCode(); code != "SEARCH_ERROR" {
			t.Errorf("error code = %q, want SEARCH_ERROR", code)
		}
	})

	t.Run("primitive_per_model_cap_with_backfill", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		// 5 articles + 1 memo all match "alpha".
		for i := range 5 {
			seedArticleDoc(t, srv, "Doc "+strconv.Itoa(i), "alpha content here")
		}
		seedMemoDoc(t, srv, "alpha memo")

		// Limit 4, cap 2: fair share = 2 articles + 1 memo = 3; backfill 1 from the
		// article leftovers → 3 articles + 1 memo. The cap is a fair-chance floor,
		// not a hard ceiling, so the result fills to the limit rather than stopping
		// at 3.
		resp := srv.GET("/scoped-search?q=alpha&models=SearchArticle,SearchMemo&limit=4&per_model=2")
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 4 {
			t.Fatalf("capped+backfill returned %d, want 4: %s", got, resp.Body)
		}
		if a, m := countModel(resp, "SearchArticle"), countModel(resp, "SearchMemo"); a != 3 || m != 1 {
			t.Errorf("distribution = %d articles / %d memos, want 3 / 1", a, m)
		}
	})

	t.Run("primitive_no_cap_returns_all_matches", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		for i := range 5 {
			seedArticleDoc(t, srv, "Doc "+strconv.Itoa(i), "alpha content")
		}
		seedMemoDoc(t, srv, "alpha memo")

		resp := srv.GET("/scoped-search?q=alpha&models=SearchArticle,SearchMemo&limit=20")
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 6 {
			t.Fatalf("uncapped search returned %d, want all 6: %s", got, resp.Body)
		}
	})

	t.Run("endpoint_404_when_disabled", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{Models: searchModels()})
		srv.GETRaw("/search?q=anything").AssertStatus(http.StatusNotFound)
	})

	t.Run("endpoint_merges_across_models_by_relevance", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		seedArticleDoc(t, srv, "Postgres guide", "database tuning and vacuum")
		seedArticleDoc(t, srv, "Cooking", "pasta recipe with salt")
		seedMemoDoc(t, srv, "database backup checklist")
		seedMemoDoc(t, srv, "team standup notes")

		resp := srv.GET("/search?q=database")
		resp.AssertStatus(http.StatusOK)
		hits := resp.DataList()
		if len(hits) != 2 {
			t.Fatalf("q=database returned %d hits, want 2: %s", len(hits), resp.Body)
		}
		models := modelsOf(resp)
		if !slices.Contains(models, "SearchArticle") || !slices.Contains(models, "SearchMemo") {
			t.Errorf("merged models = %v, want both SearchArticle and SearchMemo", models)
		}
		// Every hit carries the full shape and the list is sorted by score desc.
		prev := 1e308
		for _, it := range hits {
			h := it.(map[string]any)
			if h["id"] == "" || h["snippet"] == "" {
				t.Errorf("hit missing id/snippet: %v", h)
			}
			score := h["score"].(float64)
			if score > prev {
				t.Errorf("results not sorted by score desc: %v after %v", score, prev)
			}
			prev = score
		}
	})

	t.Run("endpoint_models_subset", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		seedArticleDoc(t, srv, "Redis intro", "fast cache store")
		seedMemoDoc(t, srv, "cache invalidation memo")

		resp := srv.GET("/search?q=cache&models=SearchArticle")
		resp.AssertStatus(http.StatusOK)
		got := modelsOf(resp)
		if len(got) != 1 || got[0] != "SearchArticle" {
			t.Fatalf("models=SearchArticle returned %v, want one SearchArticle hit", got)
		}
	})

	t.Run("endpoint_limit", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		for i := range 4 {
			seedArticleDoc(t, srv, "Doc "+strconv.Itoa(i), "beta keyword")
		}
		seedMemoDoc(t, srv, "beta keyword memo")

		resp := srv.GET("/search?q=beta&limit=2")
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 2 {
			t.Fatalf("limit=2 returned %d hits, want 2", got)
		}
	})

	t.Run("endpoint_excludes_soft_deleted", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		id := seedArticleDoc(t, srv, "Ephemeral", "transient gamma content")

		srv.GET("/search?q=gamma").AssertStatus(http.StatusOK)
		srv.DELETE("/search_articles/" + id).AssertStatus(http.StatusNoContent)

		resp := srv.GET("/search?q=gamma")
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 0 {
			t.Fatalf("soft-deleted row still searchable: %s", resp.Body)
		}
	})

	t.Run("endpoint_blank_q_is_400", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		resp := srv.GET("/search?q=")
		resp.AssertStatus(http.StatusBadRequest)
		if code := resp.ErrorCode(); code != "INVALID_QUERY" {
			t.Errorf("error code = %q, want INVALID_QUERY", code)
		}
	})

	t.Run("endpoint_unexposed_models_is_400", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		// SearchNote is searchable but not GlobalSearchable, so it cannot be named
		// on the public endpoint even though ctx.Search would accept it.
		srv.GET("/search?q=x&models=SearchNote").AssertStatus(http.StatusBadRequest)
		srv.GET("/search?q=x&models=Bogus").AssertStatus(http.StatusBadRequest)
	})

	t.Run("endpoint_punctuation_only_q_matches_nothing", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		seedArticleDoc(t, srv, "Real", "real content")
		resp := srv.GET("/search?q=%23%23%23") // "###"
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 0 {
			t.Fatalf("punctuation-only q returned %d hits, want 0", got)
		}
	})

	t.Run("endpoint_documented_in_openapi", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t)
		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			paths, _ := body["paths"].(map[string]any)
			search, ok := paths["/search"].(map[string]any)
			if !ok {
				t.Fatalf("OpenAPI spec missing /search path")
			}
			if _, ok := search["get"]; !ok {
				t.Errorf("/search path missing GET operation: %v", search)
			}
		})
	})

	t.Run("endpoint_runs_opsearch_auth_middleware", func(t *testing.T) {
		t.Parallel()
		srv := searchServer(t, func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Request.Header.Get("Authorization") == "" {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "auth required")
					return nil
				}
				return next()
			}, maniflex.ForOperation(maniflex.OpSearch))
		})
		seedArticleDoc(t, srv, "Secret", "delta content")

		srv.GET("/search?q=delta").AssertStatus(http.StatusUnauthorized)
		srv.GET("/search?q=delta", map[string]string{"Authorization": "Bearer x"}).AssertStatus(http.StatusOK)
	})
}

// TestGlobalSearchWarnsIneffectiveMiddleware verifies the startup scan that warns
// when a middleware is registered on a pipeline step its operation can never
// reach. It builds a server directly so it can capture the framework logger.
func TestGlobalSearchWarnsIneffectiveMiddleware(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	server := maniflex.New(maniflex.Config{
		Logger:             logger,
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
	})
	server.MustRegister(SearchArticle{}, maniflex.ModelConfig{GlobalSearchable: true})
	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)

	noop := func(ctx *maniflex.ServerContext, next func() error) error { return next() }

	// Ineffective: these steps do not run for the named operation.
	server.Pipeline.Service.Register(noop, maniflex.ForOperation(maniflex.OpSearch), maniflex.WithName("svc-search"))
	server.Pipeline.DB.Register(noop, maniflex.ForOperation(maniflex.OpAction), maniflex.WithName("db-action"))
	// Effective: OpCreate keeps the Service middleware useful despite the OpAction.
	server.Pipeline.Service.Register(noop, maniflex.ForOperation(maniflex.OpCreate, maniflex.OpAction), maniflex.WithName("svc-mixed"))
	// Effective: the Auth step runs for OpSearch.
	server.Pipeline.Auth.Register(noop, maniflex.ForOperation(maniflex.OpSearch), maniflex.WithName("auth-search"))

	server.EnableGlobalSearch()
	_ = server.Handler() // triggers warnIneffectiveMiddleware

	out := buf.String()
	for _, want := range []string{"svc-search", "db-action"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected ineffective-middleware warning for %q; logs:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"svc-mixed", "auth-search"} {
		if strings.Contains(out, notWant) {
			t.Errorf("did not expect a warning for %q (it is effective); logs:\n%s", notWant, out)
		}
	}
}
