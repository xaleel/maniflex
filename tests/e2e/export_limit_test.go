package e2e

// MS-PERF-B — an export reads its whole result set into memory (up to
// MaxExportRows records) and holds it until the last byte is written.
// MaxExportRows bounds one export's row count but neither how wide a row is nor
// how many exports run at once, so a few concurrent wide exports could exhaust
// the heap with every one of them individually within its cap.
// Config.MaxConcurrentExports bounds the product.

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// gateServer builds an export-enabled server whose exports block inside the
// pipeline until released, so a test can hold slots open deterministically
// rather than racing real ones.
func gateServer(t *testing.T, maxConcurrent int) (*testutil.Server, chan struct{}, *atomic.Int32) {
	t.Helper()

	gate := make(chan struct{})
	var inFlight atomic.Int32

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.ExportableRow{},
			maniflex.ModelConfig{ExportEnabled: true},
		},
		Config: func(c *maniflex.Config) {
			c.MaxConcurrentExports = maxConcurrent
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				inFlight.Add(1)
				defer inFlight.Add(-1)
				<-gate // hold the slot until the test says otherwise
				return next()
			}, maniflex.ForOperation(maniflex.OpExport))
		},
	})
	return srv, gate, &inFlight
}

// The limiter must refuse the request that would exceed the cap, rather than
// queue it — a queued export holds a connection open behind work that is slow
// by nature.
func TestExport_ConcurrencyLimited(t *testing.T) {
	const limit = 2
	srv, gate, inFlight := gateServer(t, limit)

	var wg sync.WaitGroup
	statuses := make([]int, limit)
	for i := range limit {
		wg.Go(func() {
			statuses[i] = srv.GETRaw("/exportable_rows/export").Status
		})
	}

	// Wait for both to be parked inside the pipeline, holding their slots.
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < limit {
		if time.Now().After(deadline) {
			close(gate)
			wg.Wait()
			t.Fatalf("only %d of %d exports reached the pipeline", inFlight.Load(), limit)
		}
		time.Sleep(time.Millisecond)
	}

	// Every slot is taken: the next one must be refused, not queued. Its own
	// client with a deadline, because a limiter that admits it sends it into the
	// pipeline to block on the gate forever — a hang is a far worse way to learn
	// the limiter is broken than an assertion.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.APIPath("/exportable_rows/export"))
	if err != nil {
		close(gate)
		wg.Wait()
		t.Fatalf("export beyond the %d-slot limit never answered (%v) — the limiter "+
			"admitted it into the pipeline instead of refusing it at the door", limit, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("export beyond the %d-slot limit = %d, want %d — the limiter let it "+
			"through, so nothing bounds how many result sets are live at once",
			limit, resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("a refused export carries no Retry-After header, so a client has nothing " +
			"to back off on")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "EXPORT_BUSY") {
		t.Errorf("refused export body = %s, want an EXPORT_BUSY error code", body)
	}

	close(gate)
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("export %d within the limit = %d, want 200", i, s)
		}
	}
}

// A slot must come back when the export finishes — including when it fails.
// A leaked slot would take the endpoint down permanently after MaxConcurrentExports
// requests, which is a worse outage than the one the limiter prevents.
func TestExport_SlotReleasedAfterCompletion(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.ExportableRow{},
			maniflex.ModelConfig{ExportEnabled: true},
		},
		Config: func(c *maniflex.Config) { c.MaxConcurrentExports = 1 },
	})
	seedRows(t, srv, 2)

	// Serially, well past the limit: every one must succeed.
	for i := range 5 {
		resp := srv.GETRaw("/exportable_rows/export")
		if resp.Status != http.StatusOK {
			t.Fatalf("sequential export %d = %d, want 200 — the previous export's slot "+
				"was never released", i, resp.Status)
		}
	}

	// A failing export (bad ?format) must release its slot too.
	srv.GETRaw("/exportable_rows/export?format=bogus").AssertStatus(http.StatusBadRequest)
	if resp := srv.GETRaw("/exportable_rows/export"); resp.Status != http.StatusOK {
		t.Errorf("export after a failed one = %d, want 200 — the failed export leaked its slot",
			resp.Status)
	}
}

// A negative cap means "no limit" — the escape hatch for a deployment that has
// its own admission control in front.
func TestExport_LimitDisabled(t *testing.T) {
	const n = 6
	srv, gate, inFlight := gateServer(t, -1)

	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := range n {
		wg.Go(func() {
			statuses[i] = srv.GETRaw("/exportable_rows/export").Status
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < n {
		if time.Now().After(deadline) {
			close(gate)
			wg.Wait()
			t.Fatalf("only %d of %d exports ran concurrently with the limit disabled — "+
				"a negative MaxConcurrentExports is still capping", inFlight.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("export %d with the limit disabled = %d, want 200", i, s)
		}
	}
}

// The default has to be a real limit, not "off" — an app that never sets the
// field is exactly the one that needs the ceiling.
func TestExport_DefaultLimitApplies(t *testing.T) {
	var cfg maniflex.Config
	cfg.ApplyDefaults()
	if cfg.MaxConcurrentExports != maniflex.DefaultMaxConcurrentExports {
		t.Errorf("ApplyDefaults left MaxConcurrentExports = %d, want %d",
			cfg.MaxConcurrentExports, maniflex.DefaultMaxConcurrentExports)
	}
	if maniflex.DefaultMaxConcurrentExports <= 0 {
		t.Fatal("the default export limit is not a limit")
	}
}
