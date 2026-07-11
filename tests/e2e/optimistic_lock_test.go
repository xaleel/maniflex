package e2e

// TestOptimisticLock covers roadmap item 5.9 — If-Match / ETag optimistic
// concurrency control for PATCH and DELETE.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestOptimisticLock

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// optDoc is the model used across all optimistic-lock tests.
type optDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required,filterable"`
	Notes string `json:"notes" db:"notes" mfx:"filterable"`
}

// newOptLockServer builds a test server with OptimisticLock enabled on optDoc
// and response.Cache wired so GET responses carry an ETag header.
func newOptLockServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			optDoc{},
			maniflex.ModelConfig{OptimisticLock: true},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				response.Cache(0), // max-age=0 — we only care about ETags, not caching
				maniflex.ForModel("optDoc"),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})
}

// ── Happy-path ────────────────────────────────────────────────────────────────

func TestOptimisticLock_PatchWithCorrectETagSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "original"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not return an ETag header")
	}

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "updated"},
		map[string]string{"If-Match": etag}).
		AssertStatus(http.StatusOK)
}

func TestOptimisticLock_DeleteWithCorrectETagSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "to-delete"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not return an ETag header")
	}

	srv.DELETE("/opt_docs/"+id, map[string]string{"If-Match": etag}).
		AssertStatus(http.StatusNoContent)
}

// ── 412 on stale ETag ─────────────────────────────────────────────────────────

func TestOptimisticLock_PatchWithWrongETagReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"},
		map[string]string{"If-Match": `"not-the-right-etag"`}).
		AssertStatus(http.StatusPreconditionFailed)
}

func TestOptimisticLock_DeleteWithWrongETagReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.DELETE("/opt_docs/"+id, map[string]string{"If-Match": `"wrong"`}).
		AssertStatus(http.StatusPreconditionFailed)
}

func TestOptimisticLock_412ResponseHasPreconditionFailedCode(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "doc"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	resp := srv.PATCH("/opt_docs/"+id, map[string]any{"notes": "x"},
		map[string]string{"If-Match": `"stale"`})
	resp.AssertStatus(http.StatusPreconditionFailed)
	resp.AssertJSON(func(body map[string]any) {
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected 'error' object, got %T", body["error"])
		}
		if errObj["code"] != "PRECONDITION_FAILED" {
			t.Errorf("error.code: got %q, want PRECONDITION_FAILED", errObj["code"])
		}
	})
}

// ── No If-Match header → no enforcement ──────────────────────────────────────

func TestOptimisticLock_PatchWithoutIfMatchSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"}).
		AssertStatus(http.StatusOK)
}

func TestOptimisticLock_DeleteWithoutIfMatchSucceeds(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	srv.DELETE("/opt_docs/" + id).AssertStatus(http.StatusNoContent)
}

// ── ETag freshness ────────────────────────────────────────────────────────────

func TestOptimisticLock_ETagChangesAfterUpdate(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag1 := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"},
		map[string]string{"If-Match": etag1}).
		AssertStatus(http.StatusOK)
	etag2 := srv.GET("/opt_docs/" + id).Header.Get("ETag")

	if etag1 == etag2 {
		t.Error("ETag must change after an update")
	}
}

func TestOptimisticLock_StaleETagAfterUpdateReturns412(t *testing.T) {
	t.Parallel()
	srv := newOptLockServer(t)

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	staleETag := srv.GET("/opt_docs/" + id).Header.Get("ETag")

	// A concurrent writer updates the record.
	srv.PATCH("/opt_docs/"+id, map[string]any{"title": "v2"}).
		AssertStatus(http.StatusOK)

	// Original ETag is now stale — must get 412.
	srv.PATCH("/opt_docs/"+id, map[string]any{"notes": "late update"},
		map[string]string{"If-Match": staleETag}).
		AssertStatus(http.StatusPreconditionFailed)
}

// ── Concurrent writers holding the same ETag ─────────────────────────────────

// updateBarrier holds the first writer to reach the UPDATE statement until a
// second one arrives, or until it gives up after a grace period. It is what
// makes the lost-update race deterministic instead of a microsecond-wide
// coin flip:
//
//   - Check-then-act: the loser's precondition check runs against the row the
//     winner has not written yet, so it passes and issues its own UPDATE. Both
//     writers meet at the barrier and both clobber their way to a 200.
//   - Atomic check-and-write: the loser is still blocked on the row lock the
//     winner's transaction holds and can never reach an UPDATE. The winner waits
//     out the grace period alone, commits, and the loser then re-reads a record
//     whose ETag has moved on — 412.
type updateBarrier struct {
	arrived atomic.Int32
	both    chan struct{}
	once    sync.Once
}

func (b *updateBarrier) wait() {
	if b.arrived.Add(1) >= 2 {
		b.once.Do(func() { close(b.both) })
		return
	}
	select {
	case <-b.both:
	case <-time.After(500 * time.Millisecond): // no second writer — it is stuck on our lock
	}
}

// gatedAdapter routes every UPDATE through the barrier, whether the write runs
// on the bare adapter or inside a transaction. Everything else is the embedded
// SQLite adapter's own behaviour.
type gatedAdapter struct {
	maniflex.DBAdapter
	barrier *updateBarrier
}

func (a gatedAdapter) Update(ctx context.Context, model *maniflex.ModelMeta, id string,
	record any, present map[string]struct{}) (any, error) {
	a.barrier.wait()
	return a.DBAdapter.Update(ctx, model, id, record, present)
}

func (a gatedAdapter) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	tx, err := a.DBAdapter.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return gatedTx{Tx: tx, barrier: a.barrier}, nil
}

type gatedTx struct {
	maniflex.Tx
	barrier *updateBarrier
}

func (t gatedTx) Update(ctx context.Context, model *maniflex.ModelMeta, id string,
	record any, present map[string]struct{}) (any, error) {
	t.barrier.wait()
	return t.Tx.Update(ctx, model, id, record, present)
}

// Two clients read the same ETag and PATCH concurrently. The precondition check
// and the write it guards are one row-locked transaction, so exactly one wins:
// the loser blocks on the lock until the winner commits, then re-reads a record
// whose ETag has moved and gets its 412 — instead of validating against the
// pre-write row and silently overwriting the winner (BUG-2).
func TestOptimisticLock_ConcurrentSameETagOnlyOneWins(t *testing.T) {
	t.Parallel()

	barrier := &updateBarrier{both: make(chan struct{})}
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{
			optDoc{},
			maniflex.ModelConfig{OptimisticLock: true},
		},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			return gatedAdapter{DBAdapter: inner, barrier: barrier}, nil
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				response.Cache(0),
				maniflex.ForModel("optDoc"),
				maniflex.ForOperation(maniflex.OpRead),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	id := srv.POST("/opt_docs", map[string]any{"title": "v1"}).
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	etag := srv.GET("/opt_docs/" + id).Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not return an ETag header")
	}

	titles := [2]string{"writer-a", "writer-b"}
	var statuses [2]int
	var wg sync.WaitGroup
	for i, title := range titles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = srv.PATCH("/opt_docs/"+id, map[string]any{"title": title},
				map[string]string{"If-Match": etag}).Status
		}()
	}
	wg.Wait()

	var won, conflicted int
	for _, st := range statuses {
		switch st {
		case http.StatusOK:
			won++
		case http.StatusPreconditionFailed:
			conflicted++
		default:
			t.Fatalf("unexpected status %d, want 200 or 412", st)
		}
	}
	if won != 1 || conflicted != 1 {
		t.Fatalf("statuses %v: got %d×200 and %d×412, want exactly one of each "+
			"(two winners means the second write clobbered the first)", statuses, won, conflicted)
	}

	// The record carries the winner's title — the loser's write never landed.
	if got := srv.GET("/opt_docs/" + id).Data()["title"]; got != titles[0] && got != titles[1] {
		t.Fatalf("title = %v, want one of %v", got, titles)
	}
}

// ── Opt-out: model without OptimisticLock ignores If-Match ────────────────────

func TestOptimisticLock_ModelWithoutFlagIgnoresIfMatch(t *testing.T) {
	t.Parallel()
	// Default test server uses the standard fixtures which do NOT set OptimisticLock.
	srv := testutil.NewServer(t, testutil.Options{})

	id := srv.CreateUser("Alice", "alice-optlock@test.com", "viewer").
		AssertStatus(http.StatusCreated).Data()["id"].(string)

	// Supply a deliberately wrong ETag — should be ignored because OptimisticLock is not set.
	srv.PATCH("/users/"+id, map[string]any{"name": "Bob"},
		map[string]string{"If-Match": `"irrelevant-etag"`}).
		AssertStatus(http.StatusOK)
}
