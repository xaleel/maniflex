package maniflex

// search.go — cross-model full-text search (roadmap 4.10).
//
// Builds on the per-model FTS shipped in 4.9 (db/sqlcore/fts.go). ctx.Search
// fans the native FTS out over several mfx:"searchable" models at once and
// merges the hits into one relevance-ranked list of {model, id, snippet, score}.
//
// Like ctx.Aggregate and ctx.RecursiveQuery it builds driver-specific SQL in
// this core package and runs it through ctx.RawQuery, so it needs no addition to
// the DBAdapter interface. It reuses the same-package placeholder/quote helpers
// from aggregate.go (newAggPH/aggQuote) and duplicates the small FTS bits from
// db/sqlcore/fts.go (the <table>_fts convention, the search-language lookup, and
// the safe FTS5 match-query sanitiser) — the same trade-off aggregate.go makes
// to avoid a core→sqlcore import cycle.
//
// Scores merge correctly because a deployment uses a single driver, so every
// model shares one ranking function: Postgres ts_rank (higher = better) is used
// as-is; SQLite bm25 rank (lower = better) is negated. There is no attempt to
// normalise across drivers.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// SearchOptions configures a cross-model full-text search run by ctx.Search.
type SearchOptions struct {
	// Query is the free-form search text (the ?q= value). Blank (after trimming)
	// is a no-op: ctx.Search returns (nil, nil).
	Query string

	// Models names the registered models to search. Each must be registered and
	// declare at least one mfx:"searchable" field, else ctx.Search errors. When
	// empty, ctx.Search searches every model whose ModelConfig.GlobalSearchable
	// is set (the model set the built-in /search endpoint uses).
	Models []string

	// Limit caps the number of merged results returned. <= 0 defaults to 20.
	Limit int

	// PerModelLimit is a fairness "fair chance" cap, NOT a hard ceiling. When > 0,
	// the merge first takes at most this many of each model's top-scoring hits,
	// then backfills any slots still free up to Limit from the remaining
	// candidates by score — regardless of model — so a single model MAY exceed the
	// cap during backfill rather than leave the result short. <= 0 merges purely
	// by score.
	PerModelLimit int
}

// SearchResult is one hit in the merged cross-model search result.
type SearchResult struct {
	Model   string  `json:"model"`   // registered model name the hit came from
	ID      string  `json:"id"`      // primary key of the matched row
	Snippet string  `json:"snippet"` // highlighted excerpt of the matched text
	Score   float64 `json:"score"`   // relevance, higher = more relevant
}

// defaultSearchLimit is the merged-result cap used when SearchOptions.Limit <= 0.
const defaultSearchLimit = 20

// searchModelName is the synthetic ModelMeta name used by the built-in /search
// endpoint so model-scoped middleware (ForModel) never matches it; gate the
// endpoint with ForOperation(OpSearch) or register globally on Pipeline.Auth.
const searchModelName = "__search"

// searchSyntheticModel returns the sentinel ModelMeta for the /search endpoint,
// so pipeline steps that read ctx.Model.Name do not panic.
func searchSyntheticModel() *ModelMeta { return &ModelMeta{Name: searchModelName} }

// Search runs a cross-model full-text search and returns the merged, relevance-
// ranked hits. It participates in ctx.Tx when one is active (via ctx.RawQuery);
// otherwise it uses the adapter's read pool.
//
//	hits, err := ctx.Search(maniflex.SearchOptions{
//	    Query:  "wireless headphones",
//	    Models: []string{"Post", "Comment"}, // explicit, app-authorised set
//	    Limit:  20,
//	})
//
// With Models empty it searches every ModelConfig.GlobalSearchable model — the
// behaviour the built-in GET /search endpoint relies on.
func (c *ServerContext) Search(opts SearchOptions) ([]SearchResult, error) {
	// SearchOptions carries no per-model filters — it is a text query over a set
	// of models — so there is nowhere to put an ActionScope, and a search that
	// quietly returned every tenant's hits would be the leak the scope exists to
	// prevent. Refuse rather than run unscoped.
	if err := c.guardRaw("Search()"); err != nil {
		return nil, err
	}
	return c.search(opts)
}

func (c *ServerContext) search(opts SearchOptions) ([]SearchResult, error) {
	if c.reg == nil {
		return nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, nil
	}

	models, err := c.resolveSearchModels(opts.Models)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	driver := c.DriverType()
	perModel := make([][]SearchResult, 0, len(models))
	for _, m := range models {
		hits, err := c.searchModel(m, query, limit, driver)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			perModel = append(perModel, hits)
		}
	}

	return mergeSearch(perModel, limit, opts.PerModelLimit), nil
}

// resolveSearchModels validates an explicit model list (each must be registered
// and searchable) or, when names is empty, returns every GlobalSearchable model.
func (c *ServerContext) resolveSearchModels(names []string) ([]*ModelMeta, error) {
	if len(names) == 0 {
		var out []*ModelMeta
		for _, m := range c.reg.All() {
			if m.Config.GlobalSearchable && len(m.SearchFields) > 0 {
				out = append(out, m)
			}
		}
		return out, nil
	}

	out := make([]*ModelMeta, 0, len(names))
	for _, name := range names {
		m, ok := c.reg.Get(name)
		if !ok {
			return nil, fmt.Errorf("maniflex: model %q is not registered", name)
		}
		if len(m.SearchFields) == 0 {
			return nil, fmt.Errorf("maniflex: model %q has no mfx:\"searchable\" fields", name)
		}
		out = append(out, m)
	}
	return out, nil
}

// searchModel runs the FTS query for one model and returns its hits, already
// ordered best-first, capped at limit.
func (c *ServerContext) searchModel(m *ModelMeta, query string, limit int, driver DriverType) ([]SearchResult, error) {
	pb := newAggPH(driver)
	var sql string
	if driver == Postgres {
		sql = searchSQLPostgres(m, query, limit, pb)
	} else {
		sql = searchSQLSQLite(m, query, limit, pb)
		if sql == "" {
			// Input had no usable FTS5 tokens (e.g. punctuation only) — no matches.
			return nil, nil
		}
	}

	// rawQuery, not RawQuery: Search's own guard has already refused a scoped
	// caller by this point (or Unscoped() let one through deliberately), so the
	// public one would only re-refuse the framework's own statement.
	rows, err := c.rawQuery(sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("maniflex: search %q: %w", m.Name, err)
	}
	out := make([]SearchResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, SearchResult{
			Model:   m.Name,
			ID:      searchString(row["id"]),
			Snippet: searchString(row["snippet"]),
			Score:   searchFloat(row["score"]),
		})
	}
	return out, nil
}

// searchSQLPostgres builds the per-model search query for Postgres. The model's
// generated `fts` tsvector column (provisioned by 4.9's migrateFTS) is matched
// with websearch_to_tsquery, ranked with ts_rank, and excerpted with ts_headline
// over the concatenated searchable columns. The query text is bound three times
// (rank, headline, predicate) in the order it appears in the SQL.
func searchSQLPostgres(m *ModelMeta, query string, limit int, pb *aggPH) string {
	lang := searchLang(m)
	tbl := aggQuote(m.TableName)
	ftsCol := tbl + "." + aggQuote("fts")

	docParts := make([]string, len(m.SearchFields))
	for i, col := range m.SearchFields {
		docParts[i] = fmt.Sprintf("coalesce(%s.%s, '')", tbl, aggQuote(col))
	}
	doc := strings.Join(docParts, " || ' ' || ")

	score := fmt.Sprintf("ts_rank(%s, websearch_to_tsquery('%s', %s))",
		ftsCol, lang, pb.add(query))
	// StartSel/StopSel are set to explicitly-quoted empty strings so the snippet is
	// plain text (the default markers are <b>/</b>); the double-quoted value form is
	// the documented way to pass option values and accepts an empty string.
	snippet := fmt.Sprintf(
		`ts_headline('%s', %s, websearch_to_tsquery('%s', %s), 'StartSel="",StopSel="",MaxWords=20,MinWords=5')`,
		lang, doc, lang, pb.add(query))

	where := fmt.Sprintf("%s @@ websearch_to_tsquery('%s', %s)", ftsCol, lang, pb.add(query))
	if sd := searchSoftDeleteCond(m, Postgres); sd != "" {
		where += " AND " + sd
	}

	return fmt.Sprintf(
		"SELECT %s.%s AS id, %s AS score, %s AS snippet FROM %s WHERE %s ORDER BY score DESC LIMIT %s",
		tbl, aggQuote("id"), score, snippet, tbl, where, pb.add(limit))
}

// searchSQLSQLite builds the per-model search query for SQLite. It joins the
// external-content FTS5 shadow table (<table>_fts, from 4.9), matches with
// MATCH, scores with the negated bm25 `rank` (so higher = better, comparable to
// Postgres ts_rank), and excerpts with snippet(). Returns "" when the sanitised
// match query is empty (no usable tokens).
func searchSQLSQLite(m *ModelMeta, query string, limit int, pb *aggPH) string {
	match := sqliteSearchMatch(query)
	if match == "" {
		return ""
	}
	tbl := aggQuote(m.TableName)
	fts := aggQuote(searchFTSTable(m.TableName))

	where := fmt.Sprintf("%s MATCH %s", fts, pb.add(match))
	if sd := searchSoftDeleteCond(m, SQLite); sd != "" {
		where += " AND " + sd
	}

	// snippet(<fts>, -1, '', '', '…', 12): column -1 = pick the best matching
	// column; empty start/end markers keep the excerpt plain text.
	return fmt.Sprintf(
		"SELECT %s.%s AS id, -%s.%s AS score, snippet(%s, -1, '', '', '…', 12) AS snippet "+
			"FROM %s JOIN %s ON %s.%s = %s.%s WHERE %s ORDER BY %s.%s ASC LIMIT %s",
		tbl, aggQuote("id"), fts, aggQuote("rank"), fts,
		tbl, fts, fts, aggQuote("rowid"), tbl, aggQuote("rowid"),
		where, fts, aggQuote("rank"), pb.add(limit))
}

// mergeSearch combines each model's best-first hit lists into one relevance-
// ranked slice of at most limit results, honouring the PerModelLimit fairness cap
// (see SearchOptions.PerModelLimit). perModel slices are already score-ordered.
func mergeSearch(perModel [][]SearchResult, limit, perModelCap int) []SearchResult {
	var selected []SearchResult
	if perModelCap <= 0 {
		for _, hits := range perModel {
			selected = append(selected, hits...)
		}
	} else {
		selected = cappedMerge(perModel, limit, perModelCap)
	}

	sort.SliceStable(selected, func(i, j int) bool {
		return selected[i].Score > selected[j].Score
	})
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

// cappedMerge applies the PerModelLimit fairness cap: each model contributes up
// to perModelCap of its best hits, then any slots still free up to limit are
// backfilled from the leftovers (best-scoring first, regardless of model).
func cappedMerge(perModel [][]SearchResult, limit, perModelCap int) []SearchResult {
	var selected []SearchResult
	for _, hits := range perModel {
		selected = append(selected, hits[:min(perModelCap, len(hits))]...)
	}
	if len(selected) >= limit {
		return selected
	}

	var leftovers []SearchResult
	for _, hits := range perModel {
		if len(hits) > perModelCap {
			leftovers = append(leftovers, hits[perModelCap:]...)
		}
	}
	sort.SliceStable(leftovers, func(i, j int) bool {
		return leftovers[i].Score > leftovers[j].Score
	})
	return append(selected, leftovers[:min(limit-len(selected), len(leftovers))]...)
}

// ── Duplicated FTS helpers (mirror db/sqlcore/fts.go) ──────────────────────────

// searchFTSTable returns the SQLite FTS5 shadow-table name for a base table.
// Mirrors sqlcore.ftsTableName.
func searchFTSTable(table string) string { return table + "_fts" }

// searchLang returns the Postgres text-search configuration for a model,
// defaulting to "english". Mirrors sqlcore.searchLang; collectSearchFields
// validated it is a plain identifier, so embedding it in SQL is safe.
func searchLang(m *ModelMeta) string {
	if m.Config.SearchLanguage != "" {
		return m.Config.SearchLanguage
	}
	return "english"
}

// searchSoftDeleteCond builds the table-qualified "not soft-deleted" predicate
// for a model, or "" when the model is not soft-deletable. Mirrors
// sqlcore.softDeleteCond.
func searchSoftDeleteCond(m *ModelMeta, driver DriverType) string {
	sd := m.SoftDelete
	if !sd.Enabled {
		return ""
	}
	col := aggQuote(m.TableName) + "." + aggQuote(sd.Field)
	if sd.FieldType == SoftDeleteBool {
		if driver == Postgres {
			return col + " = FALSE"
		}
		return col + " = 0"
	}
	return col + " IS NULL"
}

// sqliteSearchMatch converts free-form user input into a safe FTS5 MATCH query:
// each run of word characters becomes a double-quoted phrase term and the terms
// are ANDed (FTS5's implicit AND). Quoting neutralises FTS5 operators so the
// query can never be a syntax error. Returns "" when the input has no usable
// tokens. Mirrors sqlcore.sqliteMatchQuery.
func sqliteSearchMatch(s string) string {
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			terms = append(terms, `"`+b.String()+`"`)
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(terms, " ")
}

// ── Result value coercion ──────────────────────────────────────────────────────

// searchString coerces a RawQuery cell to a string. scanRows already converts
// []byte to string, so the common cases are string and nil.
func searchString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// searchFloat coerces a RawQuery cell to a float64 score across the numeric/text
// shapes the two drivers can return for ts_rank / -bm25.
func searchFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}
