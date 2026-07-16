package maniflex

// url.Values is rebuilt from the raw query string by every URL.Query() call, so
// reading three parameters parsed the string three times (PERF-4). The parse is now
// memoised — lazily, so a request that never asks for a parameter parses nothing.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func queryCtx(rawQuery string) *ServerContext {
	return &ServerContext{Request: httptest.NewRequest(http.MethodGet, "/x?"+rawQuery, nil)}
}

func TestQueryParam_ReadsValues(t *testing.T) {
	t.Parallel()

	ctx := queryCtx("a=1&b=two&b=three&empty=")
	cases := map[string]string{
		"a":       "1",
		"b":       "two", // first value wins, as URL.Query().Get does
		"empty":   "",
		"missing": "",
	}
	for name, want := range cases {
		if got := ctx.QueryParam(name); got != want {
			t.Errorf("QueryParam(%q) = %q, want %q", name, got, want)
		}
	}
}

// The parse must not happen until something asks for a parameter.
func TestQueryParam_NotParsedUntilAsked(t *testing.T) {
	t.Parallel()

	ctx := queryCtx("a=1")
	if ctx.queryValues != nil {
		t.Fatal("the query string was parsed before any QueryParam call — the parse is supposed to be lazy")
	}
	_ = ctx.QueryParam("a")
	if ctx.queryValues == nil {
		t.Error("the first QueryParam call did not memoise the parse")
	}
}

// Repeat reads reuse the one parse rather than rebuilding url.Values each time.
func TestQueryParam_ParsedOnce(t *testing.T) {
	t.Parallel()

	ctx := queryCtx("a=1&b=2&c=3")

	// Writing into the memoised map is only visible to a later read if that read
	// reuses it. A fresh URL.Query() would rebuild from the raw string and lose it.
	ctx.queryParams()["injected"] = []string{"sentinel"}

	if got := ctx.QueryParam("injected"); got != "sentinel" {
		t.Errorf("QueryParam re-parsed the query string instead of reusing the memoised "+
			"values (got %q)", got)
	}
	if got := ctx.QueryParam("b"); got != "2" { // the real values still read correctly
		t.Errorf("QueryParam(\"b\") = %q, want \"2\"", got)
	}
}

// The context is shared with ctx.GoBackground goroutines, so the memo has to be
// built exactly once — same reasoning as ctx.Logger().
func TestQueryParam_ConcurrentCallersShareOneParse(t *testing.T) {
	t.Parallel()

	ctx := queryCtx("a=1")
	const n = 64
	got := make([]string, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			got[i] = ctx.QueryParam("a")
		})
	}
	close(start)
	wg.Wait()

	for i, v := range got {
		if v != "1" {
			t.Fatalf("goroutine %d read %q, want %q", i, v, "1")
		}
	}
}

// A synthesised context (NewBackground, a hand-built test ctx) has no request to
// read; QueryParam must stay total rather than nil-dereference.
func TestQueryParam_NoRequest(t *testing.T) {
	t.Parallel()

	ctx := &ServerContext{}
	if got := ctx.QueryParam("anything"); got != "" {
		t.Errorf("QueryParam on a request-less context = %q, want \"\"", got)
	}
}
