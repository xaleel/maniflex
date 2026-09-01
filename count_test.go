package maniflex

// GAP-10. An offset list pays for a COUNT over the whole filtered set on every
// page, and the client has no way to decline it — cursor mode skips the count,
// but it is not available to every model or every query (no ?q=, no arbitrary
// ?sort=). ?count=false is that decline for offset paging: no total, no pages,
// and has_more in their place so the client can still render a next-page control.
//
//	go test . -run 'Count|Uncounted'

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryParams_WantsTotal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		q    *QueryParams
		want bool
		why  string
	}{
		{
			name: "zero value counts",
			q:    &QueryParams{Page: 1, Limit: 20},
			want: true,
			why: "every QueryParams built by hand — cascade's restrict guard, the " +
				"history reads, a middleware's own list — leaves this field alone, " +
				"and they all read the total",
		},
		{
			name: "count=false suppresses",
			q:    &QueryParams{Page: 1, Limit: 20, SkipCount: true},
			want: false,
			why:  "the request declined the count",
		},
		{
			name: "cursor mode never counts",
			q:    &QueryParams{Limit: 20, Cursor: &CursorParams{Field: "seq"}},
			want: false,
			why:  "keyset pagination reports has_more instead; that predates GAP-10",
		},
		{
			name: "cursor and count=false agree",
			q:    &QueryParams{Limit: 20, SkipCount: true, Cursor: &CursorParams{Field: "seq"}},
			want: false,
			why:  "asking to skip what is already skipped is not a contradiction",
		},
		{
			name: "nil params count",
			q:    nil,
			want: true,
			why:  "an adapter handed nil must not silently drop the total",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.WantsTotal(); got != tc.want {
				t.Errorf("WantsTotal() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestParseQueryParams_Count(t *testing.T) {
	t.Parallel()

	parse := func(t *testing.T, url string) (*QueryParams, error) {
		t.Helper()
		return ParseQueryParams(httptest.NewRequest("GET", url, nil), arithmeticTestModel(), nil)
	}

	t.Run("absent leaves the count on", func(t *testing.T) {
		t.Parallel()
		q, err := parse(t, "/arithmetic_tests")
		if err != nil {
			t.Fatalf("ParseQueryParams: %v", err)
		}
		if q.SkipCount {
			t.Error("a request that said nothing about the count must still get one")
		}
	})

	for _, raw := range []string{"false", "0", "False", "FALSE"} {
		t.Run("count="+raw+" suppresses", func(t *testing.T) {
			t.Parallel()
			q, err := parse(t, "/arithmetic_tests?count="+raw)
			if err != nil {
				t.Fatalf("ParseQueryParams: %v", err)
			}
			if !q.SkipCount {
				t.Errorf("?count=%s did not suppress the count", raw)
			}
		})
	}

	for _, raw := range []string{"true", "1"} {
		t.Run("count="+raw+" is the default spelled out", func(t *testing.T) {
			t.Parallel()
			q, err := parse(t, "/arithmetic_tests?count="+raw)
			if err != nil {
				t.Fatalf("ParseQueryParams: %v", err)
			}
			if q.SkipCount {
				t.Errorf("?count=%s must ask for the total, not skip it", raw)
			}
		})
	}

	// A value nobody meant is rejected rather than read as one of the two
	// answers. Guessing here picks a page cost for the client.
	for _, raw := range []string{"", "yes", "no", "t", "F", "2"} {
		t.Run("count="+raw+" is rejected", func(t *testing.T) {
			t.Parallel()
			if _, err := parse(t, "/arithmetic_tests?count="+raw); err == nil {
				t.Errorf("?count=%q was accepted; want a 400", raw)
			}
		})
	}
}

// ?count= and ?cursor= overlap: keyset mode has no total to give. Skipping is
// what already happens, so ?count=false agrees with it; ?count=true asks for a
// number the response cannot carry, and silently returning the cursor shape
// instead is the "quietly ignored parameter" failure this parser rejects
// everywhere else.
func TestParseQueryParams_CountWithCursor(t *testing.T) {
	t.Parallel()

	srv := New(Config{})
	if err := srv.Register(cursorOKDoc{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	model, ok := srv.Registry().Get("cursorOKDoc")
	if !ok {
		t.Fatal("model not registered")
	}

	parse := func(url string) (*QueryParams, error) {
		return ParseQueryParams(httptest.NewRequest("GET", url, nil), model, srv.Registry())
	}

	t.Run("count=false is allowed", func(t *testing.T) {
		q, err := parse("/cursor_ok_docs?cursor=&count=false")
		if err != nil {
			t.Fatalf("?cursor=&count=false rejected: %v", err)
		}
		if q.Cursor == nil {
			t.Error("cursor mode was not entered")
		}
	})

	t.Run("count=true is rejected", func(t *testing.T) {
		_, err := parse("/cursor_ok_docs?cursor=&count=true")
		if err == nil {
			t.Fatal("?cursor=&count=true was accepted; the response has no total to put in")
		}
		if !strings.Contains(err.Error(), "cursor") {
			t.Errorf("the error should name the conflicting parameter, got: %v", err)
		}
	})
}

// The uncounted page is a third meta shape. total and pages are absent rather
// than zero: a client that reads {"total": 0} off a page of twenty rows is being
// lied to, and one that reads total as "unknown" cannot, because the key is gone.
func TestResponseMeta_UncountedShape(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ResponseMeta{
		Uncounted: true,
		Page:      2,
		Limit:     20,
		HasMore:   true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"total", "pages"} {
		if _, ok := got[absent]; ok {
			t.Errorf("uncounted meta carries %q: %s", absent, raw)
		}
	}
	if got["page"] != float64(2) || got["limit"] != float64(20) {
		t.Errorf("page/limit lost: %s", raw)
	}
	if got["has_more"] != true {
		t.Errorf("has_more missing or false: %s", raw)
	}
}

func TestResponseMeta_CountedShapeUnchanged(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ResponseMeta{Total: 137, Page: 2, Limit: 20, Pages: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"total":137,"page":2,"limit":20,"pages":7}`; got != want {
		t.Errorf("the ordinary list meta changed shape:\n got %s\nwant %s", got, want)
	}
}

// The uncounted page still has to answer "is there a next page", and without a
// total the only way to know is to look. One row past the page is enough, and it
// is what cursor mode already does for the same reason.
func TestOverFetch(t *testing.T) {
	t.Parallel()

	t.Run("adds one row when the count was declined", func(t *testing.T) {
		t.Parallel()
		q := &QueryParams{Page: 3, Limit: 20, SkipCount: true}
		got := overFetch(q)
		if got == q {
			t.Fatal("overFetch mutated the request's own query instead of copying it")
		}
		if got.Limit != 21 {
			t.Errorf("Limit = %d, want 21", got.Limit)
		}
		if got.Page != 3 || !got.SkipCount {
			t.Errorf("the copy lost the rest of the query: %+v", got)
		}
		if q.Limit != 20 {
			t.Errorf("the original query's Limit became %d; meta.limit reports it", q.Limit)
		}
	})

	// The offset is derived from Page and Limit, so widening Limit for the probe
	// moves the window as well as growing it: page 2 of 2 would start at row 3
	// instead of row 2 and the client would silently lose a row per page.
	t.Run("keeps the page's own offset", func(t *testing.T) {
		t.Parallel()
		q := &QueryParams{Page: 2, Limit: 2, SkipCount: true}
		if got, want := overFetch(q).Offset(), q.Offset(); got != want {
			t.Errorf("probe offset = %d, want %d", got, want)
		}
	})

	t.Run("leaves a counted list alone", func(t *testing.T) {
		t.Parallel()
		q := &QueryParams{Page: 1, Limit: 20}
		if got := overFetch(q); got != q {
			t.Errorf("a list with a total does not need the probe row: Limit = %d", got.Limit)
		}
	})

	t.Run("leaves cursor mode alone", func(t *testing.T) {
		t.Parallel()
		q := &QueryParams{Limit: 20, SkipCount: true, Cursor: &CursorParams{Field: "seq"}}
		if got := overFetch(q); got != q {
			t.Error("cursor mode already over-fetches its own row; doing it twice " +
				"would build the next-page token from a row the client never saw")
		}
	})

	// A hand-built query can carry any Limit at all. Limit+1 wrapping negative
	// would reach the adapter as LIMIT -1.
	t.Run("does not overflow a saturated limit", func(t *testing.T) {
		t.Parallel()
		maxInt := int(^uint(0) >> 1)
		q := &QueryParams{Page: 1, Limit: maxInt, SkipCount: true}
		if got := overFetch(q); got.Limit <= 0 {
			t.Errorf("Limit = %d", got.Limit)
		}
	})
}

func TestTrimOverFetch(t *testing.T) {
	t.Parallel()

	rows := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = i
		}
		return out
	}

	t.Run("a full page plus the probe row has more", func(t *testing.T) {
		t.Parallel()
		got, more := trimOverFetch(rows(4), &QueryParams{Page: 1, Limit: 3, SkipCount: true})
		if len(got) != 3 {
			t.Errorf("returned %d rows, want 3: the probe row must not reach the client", len(got))
		}
		if !more {
			t.Error("has_more = false with a row waiting past the page")
		}
	})

	t.Run("an exactly full page is the last page", func(t *testing.T) {
		t.Parallel()
		got, more := trimOverFetch(rows(3), &QueryParams{Page: 1, Limit: 3, SkipCount: true})
		if len(got) != 3 {
			t.Errorf("returned %d rows, want 3", len(got))
		}
		if more {
			t.Error("has_more = true when the over-fetch found nothing past the page")
		}
	})

	t.Run("a short page is the last page", func(t *testing.T) {
		t.Parallel()
		got, more := trimOverFetch(rows(1), &QueryParams{Page: 1, Limit: 3, SkipCount: true})
		if len(got) != 1 || more {
			t.Errorf("got %d rows, has_more=%v; want 1, false", len(got), more)
		}
	})

	t.Run("a counted list is never trimmed", func(t *testing.T) {
		t.Parallel()
		got, more := trimOverFetch(rows(3), &QueryParams{Page: 1, Limit: 3})
		if len(got) != 3 || more {
			t.Errorf("got %d rows, has_more=%v; want 3, false", len(got), more)
		}
	})
}

// An export streams rows to a file. It has no meta block, so nothing ever read
// the total — and the COUNT it paid for ran over the same filtered set the
// export was about to walk, on the largest result sets the framework serves.
func TestExportQuery_DeclinesTheCountNobodyReads(t *testing.T) {
	t.Parallel()

	model := &ModelMeta{Name: "Order", TableName: "orders"}
	requested := &QueryParams{Page: 4, Limit: 20, Search: "widget", Includes: []string{"user"}}

	q, limit := exportQuery(model, requested)

	if q.WantsTotal() {
		t.Error("an export still runs the COUNT behind meta.total, and an export has no meta")
	}
	if limit != DefaultMaxExportRows {
		t.Errorf("row cap = %d, want %d", limit, DefaultMaxExportRows)
	}
	if q.Limit != DefaultMaxExportRows+1 {
		t.Errorf("Limit = %d, want the cap plus the overrun sentinel (%d)", q.Limit, DefaultMaxExportRows+1)
	}
	if q.Page != 1 {
		t.Errorf("Page = %d: an export is not paginated", q.Page)
	}
	if q.Search != "widget" || len(q.Includes) != 1 {
		t.Errorf("the export dropped part of the request: %+v", q)
	}
}

func TestExportQuery_HonoursTheModelsRowCap(t *testing.T) {
	t.Parallel()

	model := &ModelMeta{Name: "Order", TableName: "orders"}
	model.Config.MaxExportRows = 7

	q, limit := exportQuery(model, &QueryParams{Page: 1, Limit: 20})
	if limit != 7 || q.Limit != 8 {
		t.Errorf("cap = %d, Limit = %d; want 7 and 8", limit, q.Limit)
	}
}

// A parameter a generated client cannot see is a parameter nobody sends.
func TestOpenAPI_AdvertisesCount(t *testing.T) {
	t.Parallel()

	model := arithmeticTestModel()

	var found *OASParameter
	for i, p := range listParameters(model) {
		if p.Name == "count" {
			found = &listParameters(model)[i]
		}
	}
	if found == nil {
		t.Fatal("list OpenAPI parameters omit count")
	}
	if found.In != "query" || found.Schema == nil || found.Schema.Type != "boolean" {
		t.Errorf("count parameter = %+v, want a boolean query parameter", found)
	}

	// An export overrides pagination and never carries a meta block, so count
	// belongs with page and limit among the parameters it does not accept.
	for _, p := range exportQueryParameters(model) {
		if p.Name == "count" {
			t.Error("the export operation advertises count, which it ignores")
		}
	}
}

// The list envelope's meta is a union of three shapes. The schema documented
// only the offset one, so a generated client had no field for has_more — which
// both cursor mode and ?count=false return in place of the total.
func TestOpenAPI_ListMetaCoversEveryShape(t *testing.T) {
	t.Parallel()

	meta := listEnvelopeSchema("Order").Properties["meta"]
	if meta == nil {
		t.Fatal("list envelope has no meta")
	}
	for _, key := range []string{"total", "page", "limit", "pages", "has_more", "next_cursor"} {
		if _, ok := meta.Properties[key]; !ok {
			t.Errorf("meta schema omits %q", key)
		}
	}
}
