# Searching

maniflex has two layers of full-text search:

- **Per-model search** — the `?q=` parameter on a model's list endpoint, over its
  `mfx:"searchable"` fields. Covered in [Querying](querying.md#q-full-text-search).
- **Cross-model search** — search several models at once and merge the hits into
  one relevance-ranked list. That is what this page documents: the `ctx.Search`
  primitive and the built-in `GET /search` endpoint.

Both use the database's native full-text engine (PostgreSQL `tsvector` /
ts_rank, SQLite FTS5 / bm25), provisioned automatically for every model that
declares `mfx:"searchable"` fields. Free-form input is sanitised, so a query can
never be a syntax error.

## The `ctx.Search` primitive

`ctx.Search` runs a cross-model search and returns the merged, relevance-ranked
hits. Use it from a custom [Action](../advanced-topics/actions.md) to build search
endpoints scoped to exactly the models you choose, with your own authorisation:

```go
server.Action(maniflex.ActionConfig{
    Method: "GET", Path: "/search-community",
    Middleware: []maniflex.MiddlewareFunc{communityAuth},
    Handler: func(ctx *maniflex.ServerContext) error {
        hits, err := ctx.Search(maniflex.SearchOptions{
            Query:  ctx.QueryParam("q"),
            Models: []string{"Post", "Comment"}, // explicit, app-authorised set
            Limit:  20,
        })
        if err != nil {
            ctx.Abort(400, "SEARCH_ERROR", err.Error())
            return nil
        }
        ctx.Response = &maniflex.APIResponse{StatusCode: 200, Data: hits}
        return nil
    },
})
```

```go
type SearchOptions struct {
    Query         string   // the search text; blank → no-op (no results)
    Models        []string // models to search; empty → all GlobalSearchable models
    Limit         int      // max merged results; <= 0 → 20
    PerModelLimit int      // fairness cap (see Merge order); <= 0 → pure relevance
}

type SearchResult struct {
    Model   string  `json:"model"`   // the model the hit came from
    ID      string  `json:"id"`      // primary key of the matched row
    Snippet string  `json:"snippet"` // excerpt of the matched text
    Score   float64 `json:"score"`   // relevance, higher = more relevant
}

func (c *ServerContext) Search(opts SearchOptions) ([]SearchResult, error)
```

With an explicit `Models` list each named model only needs `mfx:"searchable"`
fields — it does **not** need `GlobalSearchable`. That flag governs only the
built-in endpoint below; the Action path is yours to authorise. `ctx.Search`
participates in `ctx.Tx` when one is active and excludes soft-deleted rows.

## The built-in `GET /search` endpoint

Enable it explicitly, then opt models in with `ModelConfig.GlobalSearchable`:

```go
server.EnableGlobalSearch() // mounts GET {PathPrefix}/search

server.MustRegister(
    Post{},    maniflex.ModelConfig{GlobalSearchable: true},
    Comment{}, maniflex.ModelConfig{GlobalSearchable: true},
    Product{}, maniflex.ModelConfig{GlobalSearchable: true},
)
```

`GlobalSearchable` requires the model to declare at least one `mfx:"searchable"`
field; registration fails otherwise.

```
GET /api/search?q=wireless+headphones
GET /api/search?q=invoice&limit=10&models=Product
```

| Parameter | Default | Notes |
|---|---|---|
| `q` | — | Required. Blank → `400 INVALID_QUERY`. |
| `limit` | `20` | Clamped to the configured maximum (default `100`). |
| `models` | all | Comma-separated subset; each name must be a `GlobalSearchable` model, else `400`. |

The response is the standard envelope with a flat array of hits, ordered by
score descending:

```json
{
  "data": [
    {"model": "Product", "id": "9f8…", "snippet": "wireless …", "score": 0.61},
    {"model": "Post",    "id": "1a2…", "snippet": "… wireless", "score": 0.18}
  ]
}
```

Configure the route and limits via `EnableGlobalSearch`:

```go
server.EnableGlobalSearch(maniflex.GlobalSearchConfig{
    Path:         "/search",
    DefaultLimit: 20,
    MaxLimit:     100,
})
```

### Authorization

The endpoint runs only the global **Auth** pipeline step — it does **not** apply
per-model auth or tenancy middleware. Gate it with `Pipeline.Auth` middleware,
either globally or scoped to the search operation:

```go
server.Pipeline.Auth.Register(requireLogin, maniflex.ForOperation(maniflex.OpSearch))
```

Because per-model row-level rules are not applied, only set `GlobalSearchable` on
models that are safe to expose this way. When you need per-model authorisation,
build a scoped Action with `ctx.Search` instead (see above) and attach your own
middleware. Middleware registered for `OpSearch` on the Deserialize, Validate,
Service, or DB steps never runs (the endpoint skips them) and is reported with a
startup warning.

## Merge order

A deployment uses one database driver, so every model shares one ranking
function and the scores are directly comparable; results merge by score
descending.

By default the merge is pure relevance — if one model's hits dominate, the
result can be entirely from that model. Set `PerModelLimit` to give each model a
fair share: the merge first takes up to `PerModelLimit` of each model's
top-scoring hits, then backfills any remaining slots up to `Limit` from the
leftovers (best score first, regardless of model). It is a fair-chance floor, not
a hard ceiling — the result still fills to `Limit` when some models have fewer
hits.

> Cross-model scores are a heuristic: bm25 and `ts_rank` depend on each table's
> own corpus statistics, so a common term can score higher in a table where it is
> rarer. Use `PerModelLimit` when you want guaranteed representation across
> models rather than a pure score ranking.
