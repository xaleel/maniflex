package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestQueryLimits_DefaultsAndDisableSentinel(t *testing.T) {
	t.Parallel()
	cfg := maniflex.Config{
		QueryLimits: maniflex.QueryLimits{MaxIncludes: -1},
	}
	cfg.ApplyDefaults()

	checks := map[string]int{
		"MaxURLBytes":              cfg.QueryLimits.MaxURLBytes,
		"MaxFilterClauses":         cfg.QueryLimits.MaxFilterClauses,
		"MaxFilterGroups":          cfg.QueryLimits.MaxFilterGroups,
		"MaxFiltersPerGroup":       cfg.QueryLimits.MaxFiltersPerGroup,
		"MaxSortFields":            cfg.QueryLimits.MaxSortFields,
		"MaxSelectFields":          cfg.QueryLimits.MaxSelectFields,
		"MaxAggregateSelectFields": cfg.QueryLimits.MaxAggregateSelectFields,
		"MaxAggregateGroupFields":  cfg.QueryLimits.MaxAggregateGroupFields,
		"MaxAggregateFilters":      cfg.QueryLimits.MaxAggregateFilters,
		"MaxAggregateHaving":       cfg.QueryLimits.MaxAggregateHaving,
		"MaxAggregateSortFields":   cfg.QueryLimits.MaxAggregateSortFields,
		"DefaultAggregateRows":     cfg.QueryLimits.DefaultAggregateRows,
		"MaxAggregateRows":         cfg.QueryLimits.MaxAggregateRows,
	}
	for name, value := range checks {
		t.Run(name, func(t *testing.T) {
			if value <= 0 {
				t.Errorf("%s default = %d, want a positive bound", name, value)
			}
		})
	}
	if cfg.QueryLimits.MaxIncludes != -1 {
		t.Errorf("negative disable sentinel changed to %d", cfg.QueryLimits.MaxIncludes)
	}
}

func TestQueryLimits_RequestURIHardLimit(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.QueryLimits.MaxURLBytes = 64
		},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/health")+"?padding="+strings.Repeat("x", 100), nil)
	resp.AssertStatus(http.StatusRequestURITooLong)
	if got := resp.ErrorCode(); got != "URI_TOO_LONG" {
		t.Errorf("error code: got %q, want URI_TOO_LONG", got)
	}
}

func TestQueryLimits_ListQueryShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		limits maniflex.QueryLimits
		query  url.Values
	}{
		{"filter clauses", maniflex.QueryLimits{MaxFilterClauses: 1}, url.Values{"filter": {"status:eq:draft", "title:eq:x"}}},
		{"filter groups", maniflex.QueryLimits{MaxFilterGroups: 1}, url.Values{"filter[0]": {"status:eq:draft"}, "filter[1]": {"title:eq:x"}}},
		{"clauses per group", maniflex.QueryLimits{MaxFiltersPerGroup: 1}, url.Values{"filter[0]": {"status:eq:draft", "title:eq:x"}}},
		{"sort fields", maniflex.QueryLimits{MaxSortFields: 1}, url.Values{"sort": {"title,status"}}},
		{"select fields", maniflex.QueryLimits{MaxSelectFields: 1}, url.Values{"select": {"id,title"}}},
		{"includes", maniflex.QueryLimits{MaxIncludes: 1}, url.Values{"include": {"user,comments"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewServer(t, testutil.Options{
				Config: func(cfg *maniflex.Config) {
					cfg.QueryLimits = tc.limits
				},
			})
			resp := srv.GET("/posts?" + tc.query.Encode())
			resp.AssertStatus(http.StatusBadRequest)
			if got := resp.ErrorCode(); got != "INVALID_QUERY" {
				t.Errorf("error code: got %q, want INVALID_QUERY", got)
			}
		})
	}
}

type QueryLimitedSEC10 struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"filterable"`
	Kind string `json:"kind" db:"kind" mfx:"filterable"`
}

type QueryNormalSEC10 struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"filterable"`
	Kind string `json:"kind" db:"kind" mfx:"filterable"`
}

func TestQueryLimits_ModelOverride(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			QueryLimitedSEC10{},
			maniflex.ModelConfig{QueryLimits: maniflex.QueryLimits{MaxFilterClauses: 1}},
			QueryNormalSEC10{},
		},
		Config: func(cfg *maniflex.Config) {
			cfg.QueryLimits.MaxFilterClauses = 2
		},
	})
	query := url.Values{"filter": {"name:eq:x", "kind:eq:y"}}.Encode()

	limited, _ := srv.ManiflexServer().Registry().Get("QueryLimitedSEC10")
	normal, _ := srv.ManiflexServer().Registry().Get("QueryNormalSEC10")
	if limited == nil || normal == nil {
		t.Fatal("query limit fixtures were not registered")
	}

	srv.GET("/" + limited.TableName + "?" + query).AssertStatus(http.StatusBadRequest)
	srv.GET("/" + normal.TableName + "?" + query).AssertStatus(http.StatusOK)
}
