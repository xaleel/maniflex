package e2e

// T7.3 — end-to-end read-path benchmark + allocation regression guard. A full
// in-process GET /users?limit=20 request through the real pipeline (routing + DB
// + scanStruct + marshalRecord + JSON + response), no network. Measured ~1418
// allocs/req for a 20-row page (see docs/perf/raw.txt `typed/` tag); the guard
// fails if list serialization regresses materially.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"maniflex/tests/e2e/testutil"
)

func listHandlerAndReq(tb testing.TB) (http.Handler, *http.Request) {
	tb.Helper()
	srv := testutil.NewServer(tb, testutil.Options{Models: []any{testutil.User{}}})
	for i := 0; i < 20; i++ {
		srv.CreateUser(fmt.Sprintf("User%02d", i), fmt.Sprintf("u%02d@example.com", i), "viewer").
			AssertStatus(http.StatusCreated)
	}
	return srv.ManiflexServer().Handler(), httptest.NewRequest(http.MethodGet, "/api/users?limit=20", nil)
}

func BenchmarkReadPath_ListServeHTTP(b *testing.B) {
	handler, req := listHandlerAndReq(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}

// TestReadPath_ListAllocGuard locks in the realized read-path allocation cost so
// a future change can't silently regress list serialization. The bound is the
// measured ~1418 allocs/req for a 20-row page plus generous headroom; tighten it
// if/when the struct→JSON-bytes serializer lands.
func TestReadPath_ListAllocGuard(t *testing.T) {
	handler, req := listHandlerAndReq(t)
	avg := testing.AllocsPerRun(50, func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	})
	const guard = 1900 // measured ~1418/req (20 rows); headroom for harness noise
	if avg > guard {
		t.Errorf("GET /users?limit=20 allocs/req = %.0f, guard %d — read path regressed", avg, guard)
	}
	t.Logf("GET /users?limit=20: %.0f allocs/req (20 rows, ~%.0f/row)", avg, avg/20)
}
