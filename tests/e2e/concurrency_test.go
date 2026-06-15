package e2e

// P2 #11 — concurrency guard for the typed read path. scanStruct caches a
// per-model column→field plan; concurrent scans of the same model must be
// race-free. This is most valuable under `-race` (see docs/testing-race.md for
// running the race detector on Windows via Docker/WSL), but also runs as a
// functional concurrent-list check on the default lane.

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestConcurrent_ListSameModel(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{widget{}}})

	const seeded = 20
	for i := range seeded {
		srv.POST("/widgets", map[string]any{"name": fmt.Sprintf("w%d", i), "qty": i}).
			AssertStatus(http.StatusCreated)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 5 {
				if items := srv.GET("/widgets?limit=50").DataList(); len(items) != seeded {
					t.Errorf("concurrent list: got %d rows, want %d", len(items), seeded)
				}
			}
		})
	}
	wg.Wait()
}
