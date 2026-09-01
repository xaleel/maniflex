package maniflex

// count.go — count suppression for offset lists (GAP-10).
//
// An offset list runs a COUNT over the whole filtered set to fill meta.total,
// on every page. On a large table that is routinely the more expensive half of
// the request, and a client that only pages forward never reads the number.
// Cursor mode avoids it, but cursor mode is not available to every model
// (mfx:"cursor_field") or every query (no ?q=, no arbitrary ?sort=).
//
// ?count=false is the offset-mode decline. The page then reports has_more in
// place of total and pages, which needs one row past the page to answer — the
// same over-fetch keyset pagination uses.

// overFetch returns the query to send to the adapter for q: one row longer when
// the client declined the count, so trimOverFetch can tell whether a next page
// exists, and q itself in every other case.
//
// The longer query is a copy. q is the request's own QueryParams and meta.limit
// reports its Limit, so widening it in place would tell the client its page size
// was one larger than the page it received.
func overFetch(q *QueryParams) *QueryParams {
	if !probing(q) || q.Limit == maxInt {
		return q
	}
	probe := *q
	probe.Limit = q.Limit + 1
	// Pinned, because Offset() derives from Page and Limit: a wider Limit would
	// start the page one row later as well as reading one row further, and the
	// client would lose a row per page.
	probe.offset = q.Offset()
	return &probe
}

// trimOverFetch drops the probe row overFetch asked for and reports whether it
// arrived. A page with the probe row has a next page; one without it is the last.
func trimOverFetch(items []any, q *QueryParams) ([]any, bool) {
	if !probing(q) || len(items) <= q.Limit {
		return items, false
	}
	return items[:q.Limit], true
}

// probing reports whether q is an offset page that declined its count, the only
// shape the probe row applies to. Cursor mode is excluded because it over-fetches
// a row of its own: probing on top of that would build the next-page token from a
// row the client never sees.
func probing(q *QueryParams) bool {
	return q != nil && q.SkipCount && q.Cursor == nil && q.Limit > 0
}

// maxInt is the saturation point Offset() also guards against: a hand-built
// query can carry any Limit, and Limit+1 wrapping negative would reach the
// adapter as LIMIT -1.
const maxInt = int(^uint(0) >> 1)

// exportQuery builds the query behind GET /{table}/export from the one the
// request parsed, and returns it with the row cap it was built against.
// An export is not paginated: it reads the whole filtered set up to the cap,
// plus one row so an overrun can be detected and refused rather than truncated
// into a file that looks complete.
//
// It declines the count for the plainest reason there is — an export writes rows
// and no meta block, so nothing ever read the total. The COUNT it used to run
// scanned the same filtered set the export was about to walk, which is the
// largest one the framework serves.
func exportQuery(model *ModelMeta, q *QueryParams) (*QueryParams, int) {
	rows := model.Config.MaxExportRows
	if rows <= 0 {
		rows = DefaultMaxExportRows
	}
	return &QueryParams{
		Page:      1,
		Limit:     rows + 1, // +1 sentinel so we can detect overrun
		SkipCount: true,
		Filters:   q.Filters,
		Sorts:     q.Sorts,
		Includes:  q.Includes,
		Fields:    q.Fields,
		Search:    q.Search, // honour ?q= full-text search on exports too
	}, rows
}
