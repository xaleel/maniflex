package e2e

// R7 — batch-resolved computed fields, bounded per-row fan-out, and OpenAPI
// visibility. Uses LockedInvoice (number, status, amount) like computed_field_test.go.

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// mkInvoice creates a LockedInvoice and returns its id.
func mkInvoice(t *testing.T, srv *testutil.Server, number string, amount int) string {
	t.Helper()
	resp := srv.POST("/locked_invoices", map[string]any{
		"number": number, "status": "draft", "amount": amount,
	})
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// ── the headline: one call per page, not one per row ────────────────────────

func TestBatchComputed_ResolvesWholePageInOneCall(t *testing.T) {
	t.Parallel()
	var calls, rowsSeen atomic.Int64
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "double_amount",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				calls.Add(1)
				rowsSeen.Add(int64(len(rows)))
				out := make([]any, len(rows))
				for i, r := range rows {
					out[i] = jsonInt(r["amount"]) * 2
				}
				return out, nil
			})
	})

	for i := range 5 {
		mkInvoice(t, srv, fmt.Sprintf("INV-B-%d", i), (i+1)*10)
	}
	calls.Store(0)
	rowsSeen.Store(0)

	list := srv.GET("/locked_invoices?limit=50", nil)
	list.AssertStatus(http.StatusOK)
	items := list.DataList()
	if len(items) != 5 {
		t.Fatalf("got %d rows, want 5", len(items))
	}

	// The whole point of R7: N rows cost ONE resolver call, not N.
	if n := calls.Load(); n != 1 {
		t.Fatalf("resolver called %d times for a 5-row page, want 1 — that is the N+1 R7 exists to remove", n)
	}
	if n := rowsSeen.Load(); n != 5 {
		t.Fatalf("resolver saw %d rows, want the whole page (5)", n)
	}

	// Row order is unspecified (no sortable column), so check each row against
	// its own amount rather than a positional expectation.
	for _, it := range items {
		row := it.(map[string]any)
		want := jsonInt(row["amount"]) * 2
		if got := jsonInt(row["double_amount"]); got != want {
			t.Errorf("%v double_amount: got %v, want %v", row["number"], got, want)
		}
	}
}

// Values must land on the row they were computed for, in order.
func TestBatchComputed_ValuesAlignToTheirRows(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "echo_number",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				out := make([]any, len(rows))
				for i, r := range rows {
					out[i] = "echo:" + r["number"].(string)
				}
				return out, nil
			})
	})

	for i := range 4 {
		mkInvoice(t, srv, fmt.Sprintf("INV-A-%d", i), i)
	}

	list := srv.GET("/locked_invoices?limit=50", nil)
	list.AssertStatus(http.StatusOK)
	items := list.DataList()
	if len(items) != 4 {
		t.Fatalf("got %d rows, want 4", len(items))
	}
	for _, it := range items {
		row := it.(map[string]any)
		want := "echo:" + row["number"].(string)
		if got := row["echo_number"]; got != want {
			t.Errorf("misaligned: row %v got echo_number %v, want %v", row["number"], got, want)
		}
	}
}

// A single read and the create echo call the batch fn with a one-row slice, so
// one registration serves every read path.
func TestBatchComputed_SingleReadAndCreateEcho(t *testing.T) {
	t.Parallel()
	var maxRows atomic.Int64
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "double_amount",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				maxRows.Store(int64(len(rows)))
				out := make([]any, len(rows))
				for i, r := range rows {
					out[i] = jsonInt(r["amount"]) * 2
				}
				return out, nil
			})
	})

	// create echo
	resp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-S-1", "status": "draft", "amount": 21,
	})
	resp.AssertStatus(http.StatusCreated)
	if got, _ := resp.Data()["double_amount"].(float64); got != 42 {
		t.Errorf("create echo double_amount: got %v, want 42", resp.Data()["double_amount"])
	}
	if n := maxRows.Load(); n != 1 {
		t.Errorf("create echo passed %d rows, want a 1-row slice", n)
	}

	// single read
	id := resp.ID()
	read := srv.GET("/locked_invoices/"+id, nil)
	read.AssertStatus(http.StatusOK)
	if got, _ := read.Data()["double_amount"].(float64); got != 42 {
		t.Errorf("read double_amount: got %v, want 42", read.Data()["double_amount"])
	}
}

// A batch resolver gets the *ServerContext, so it can reach the DB it is
// batching against — the whole reason the signature changed.
func TestBatchComputed_ReceivesServerContext(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "sibling_count",
			func(ctx *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				// One query for the whole page, through ctx — not an external handle.
				all, err := ctx.GetModel("LockedInvoice").List(&maniflex.QueryParams{Page: 1, Limit: 100})
				if err != nil {
					return nil, err
				}
				out := make([]any, len(rows))
				for i := range rows {
					out[i] = len(all)
				}
				return out, nil
			})
	})

	for i := range 3 {
		mkInvoice(t, srv, fmt.Sprintf("INV-C-%d", i), i)
	}

	list := srv.GET("/locked_invoices?limit=50", nil)
	list.AssertStatus(http.StatusOK)
	for _, it := range list.DataList() {
		if got, _ := it.(map[string]any)["sibling_count"].(float64); got != 3 {
			t.Errorf("sibling_count: got %v, want 3", got)
		}
	}
}

// ── failure handling ────────────────────────────────────────────────────────

// A wrong-length return would write values onto the wrong records. Refuse the
// whole field instead — an absent column is diagnosable, a misaligned one is not.
func TestBatchComputed_WrongLengthOmitsFieldRatherThanMisalign(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "bad",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				return []any{"only-one"}, nil // short slice
			})
	})

	for i := range 3 {
		mkInvoice(t, srv, fmt.Sprintf("INV-W-%d", i), i)
	}

	list := srv.GET("/locked_invoices?limit=50", nil)
	list.AssertStatus(http.StatusOK)
	for _, it := range list.DataList() {
		row := it.(map[string]any)
		if v, present := row["bad"]; present {
			t.Fatalf("a short return must omit the field entirely, not land %v on some row", v)
		}
	}
}

func TestBatchComputed_ErrorOmitsFieldButKeepsResponse(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "boom",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				return nil, errors.New("resolver exploded")
			})
	})
	mkInvoice(t, srv, "INV-E-1", 5)

	list := srv.GET("/locked_invoices?limit=50", nil)
	list.AssertStatus(http.StatusOK) // a bad resolver must not poison the response
	row := list.DataList()[0].(map[string]any)
	if _, present := row["boom"]; present {
		t.Error("a failed batch resolver must omit its field")
	}
	if row["number"] != "INV-E-1" {
		t.Error("the rest of the record must survive a failed resolver")
	}
}

// ── the fan-out bound ───────────────────────────────────────────────────────

// A per-row resolver used to get one goroutine per row with no ceiling: a
// 100-row page fired 100 concurrent round-trips.
func TestComputed_PerRowFanOutIsBounded(t *testing.T) {
	t.Parallel()
	var inFlight, peak atomic.Int64
	var blocking atomic.Bool // the create echo resolves too; only gate the list
	var mu sync.Mutex
	release := make(chan struct{})

	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "slow",
			func(_ *maniflex.ServerContext, _ map[string]any) (any, error) {
				if !blocking.Load() {
					return 1, nil
				}
				n := inFlight.Add(1)
				mu.Lock()
				if n > peak.Load() {
					peak.Store(n)
				}
				mu.Unlock()
				<-release // hold every worker until the pool is saturated
				inFlight.Add(-1)
				return 1, nil
			})
	})

	const rows = 40
	for i := range rows {
		mkInvoice(t, srv, fmt.Sprintf("INV-F-%d", i), i)
	}

	blocking.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.GET("/locked_invoices?limit=50", nil).AssertStatus(http.StatusOK)
	}()

	// Let the pool saturate, then let everyone through.
	waitFor(t, func() bool { return inFlight.Load() >= 8 })
	close(release)
	<-done

	if p := peak.Load(); p > 8 {
		t.Fatalf("peak concurrent per-row resolvers = %d over a %d-row page, want <= 8 "+
			"(unbounded fan-out was one goroutine per row)", p, rows)
	}
}

// ── OpenAPI visibility ──────────────────────────────────────────────────────

func TestComputed_AppearsInOpenAPISpec(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddBatchComputedField("LockedInvoice", "item_count",
			func(_ *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
				return make([]any, len(rows)), nil
			},
			maniflex.ComputedSchema(&maniflex.OASSchema{Type: "integer"}))
		s.MustAddComputedField("LockedInvoice", "untyped_extra",
			func(_ *maniflex.ServerContext, _ map[string]any) (any, error) { return nil, nil })
	})

	spec := srv.GET("/openapi.json", nil)
	spec.AssertStatus(http.StatusOK)

	props := schemaProps(t, spec, "LockedInvoice")

	// Declared schema is carried through, and the field is read-only.
	ic, ok := props["item_count"].(map[string]any)
	if !ok {
		t.Fatal("computed field item_count is absent from the response schema — " +
			"a generated client would not know it exists")
	}
	if ic["type"] != "integer" {
		t.Errorf("item_count type: got %v, want integer (from ComputedSchema)", ic["type"])
	}
	if ic["readOnly"] != true {
		t.Error("a computed field must be readOnly — it is never accepted in a write body")
	}

	// No declared schema: present and read-only, but honestly untyped.
	ue, ok := props["untyped_extra"].(map[string]any)
	if !ok {
		t.Fatal("untyped computed field is absent from the response schema")
	}
	if _, hasType := ue["type"]; hasType {
		t.Errorf("a computed field with no ComputedSchema must not claim a type; got %v", ue["type"])
	}
	if ue["readOnly"] != true {
		t.Error("untyped computed field must still be readOnly")
	}

	// Computed fields must never appear in write bodies.
	for _, schema := range []string{"LockedInvoiceCreate", "LockedInvoiceUpdate"} {
		w := schemaProps(t, spec, schema)
		for _, name := range []string{"item_count", "untyped_extra"} {
			if _, present := w[name]; present {
				t.Errorf("%s must not carry computed field %q — it is not writable", schema, name)
			}
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the resolver pool to saturate")
}

// schemaProps digs components.schemas.<name>.properties out of the spec.
func schemaProps(t *testing.T, spec *testutil.Response, name string) map[string]any {
	t.Helper()
	var props map[string]any
	spec.AssertJSON(func(body map[string]any) {
		comps, _ := body["components"].(map[string]any)
		schemas, _ := comps["schemas"].(map[string]any)
		s, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("schema %q missing from spec", name)
		}
		props, _ = s["properties"].(map[string]any)
	})
	if props == nil {
		t.Fatalf("schema %q has no properties", name)
	}
	return props
}

// jsonInt coerces a value that may be a scanned int64 (the resolver sees the
// pre-marshal row) or a JSON float64 (the client sees the decoded response).
// The package-level toInt deliberately rejects float64, so it cannot be reused.
func jsonInt(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}
