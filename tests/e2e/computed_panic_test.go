package e2e

// Audit MS-6: a panicking computed field took the whole process down on list
// endpoints. Per-row callbacks fan out across a worker pool, and a panic in a
// goroutine no one recovers is unrecoverable — PanicRecoverer only wraps the
// request goroutine. A single-row read runs the same callback inline, so it
// recovered into a 500; the same field on a two-row page killed the server.
//
// computeRow's own contract says a failure must not poison the record: an error
// is logged and the field omitted. A panic now follows the same rule.
//
//	go test ./tests/e2e/... -run TestComputedPanic

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type cpDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name"`
}

// cpServer registers one computed field that panics for every row, and one that
// works. The healthy field is what proves containment: a panic must cost its own
// field and nothing else.
func cpServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{cpDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.AddComputedField("cpDoc", "boom",
				func(ctx *maniflex.ServerContext, row map[string]any) (any, error) {
					// The audit's own example: an unchecked assertion on a value
					// that is not the type the author assumed.
					return row["name"].(int), nil
				})
			s.AddComputedField("cpDoc", "healthy",
				func(ctx *maniflex.ServerContext, row map[string]any) (any, error) {
					return "ok", nil
				})
		},
	})
}

func cpSeed(t *testing.T, srv *testutil.Server, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		srv.POST("/cp_docs", map[string]any{"name": "row"}).AssertStatus(http.StatusCreated)
	}
}

// The crash case: two or more rows fan out across the worker pool. Before the
// fix this did not fail the test — it killed the test binary.
func TestComputedPanic_MultiRowListSurvives(t *testing.T) {
	srv := cpServer(t)
	cpSeed(t, srv, 3)

	items := srv.GET("/cp_docs").AssertStatus(http.StatusOK).DataList()
	testutil.AssertLen(t, "rows", items, 3)

	for i, it := range items {
		row := it.(map[string]any)
		if _, present := row["boom"]; present {
			t.Errorf("row %d: the panicking field must be omitted, got %v", i, row["boom"])
		}
		if row["healthy"] != "ok" {
			t.Errorf("row %d: a panic in one computed field must not cost another; "+
				"healthy = %v", i, row["healthy"])
		}
		if row["name"] != "row" {
			t.Errorf("row %d: the record itself must survive, name = %v", i, row["name"])
		}
	}
}

// A single-row page takes the inline path rather than the pool. It must answer
// the same way, or the endpoint's behaviour depends on how many rows happen to
// match — which is exactly the discontinuity that hid this bug.
func TestComputedPanic_SingleRowListMatchesMultiRow(t *testing.T) {
	srv := cpServer(t)
	cpSeed(t, srv, 1)

	items := srv.GET("/cp_docs").AssertStatus(http.StatusOK).DataList()
	testutil.AssertLen(t, "rows", items, 1)

	row := items[0].(map[string]any)
	if _, present := row["boom"]; present {
		t.Errorf("the panicking field must be omitted, got %v", row["boom"])
	}
	if row["healthy"] != "ok" {
		t.Errorf("healthy field lost on the single-row path: %v", row["healthy"])
	}
}

// And the single-record read, which used to be the only path that degraded
// gracefully — by being recovered into a 500. It is now a 200 with the field
// omitted, consistent with the list and with computeRow's error path.
func TestComputedPanic_SingleRecordReadSurvives(t *testing.T) {
	srv := cpServer(t)
	id := srv.MustID(srv.POST("/cp_docs", map[string]any{"name": "row"}))

	data := srv.GET("/cp_docs/" + id).AssertStatus(http.StatusOK).Data()
	if _, present := data["boom"]; present {
		t.Errorf("the panicking field must be omitted, got %v", data["boom"])
	}
	if data["healthy"] != "ok" {
		t.Errorf("healthy field lost on the single-record read: %v", data["healthy"])
	}
}

// A batch field runs inline, so a panic there was never fatal — PanicRecoverer
// answered 500. But that made the blast radius depend on which registration form
// the field used: converting a per-row field to a batch one silently upgraded a
// bad row from an omitted column to a failed request.
func TestComputedPanic_BatchFieldIsContainedToo(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{cpDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.AddBatchComputedField("cpDoc", "batch_boom",
				func(ctx *maniflex.ServerContext, rows []map[string]any) ([]any, error) {
					panic("resolver blew up")
				})
			s.AddComputedField("cpDoc", "healthy",
				func(ctx *maniflex.ServerContext, row map[string]any) (any, error) {
					return "ok", nil
				})
		},
	})
	cpSeed(t, srv, 3)

	items := srv.GET("/cp_docs").AssertStatus(http.StatusOK).DataList()
	testutil.AssertLen(t, "rows", items, 3)
	for i, it := range items {
		row := it.(map[string]any)
		if _, present := row["batch_boom"]; present {
			t.Errorf("row %d: the panicking batch field must be omitted, got %v",
				i, row["batch_boom"])
		}
		if row["healthy"] != "ok" {
			t.Errorf("row %d: a batch panic must not cost an unrelated field; healthy = %v",
				i, row["healthy"])
		}
	}
}

// A page larger than the worker pool (maxComputedConcurrency is 8) exercises the
// case where every worker takes a panicking row. A recover placed on the worker
// body rather than the callback would deadlock here: each worker would unwind
// out of its receive loop, leaving the sender blocked on a channel nobody reads.
func TestComputedPanic_PageLargerThanWorkerPoolDoesNotDeadlock(t *testing.T) {
	srv := cpServer(t)
	cpSeed(t, srv, 20)

	items := srv.GET("/cp_docs?limit=20").AssertStatus(http.StatusOK).DataList()
	testutil.AssertLen(t, "rows", items, 20)
	for i, it := range items {
		if it.(map[string]any)["healthy"] != "ok" {
			t.Fatalf("row %d lost its healthy field", i)
		}
	}
}
