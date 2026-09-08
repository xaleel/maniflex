package e2e

// Audit STEP-2 — the If-Match precondition ran before the write-scope check, so
// checkOptimisticLock's findByIDForUpdate reached another tenant's row by id. The
// visible half was the 412-vs-404 oracle, covered in write_guard_scope_test.go.
//
// This is the other half the status code cannot show: the read takes a
// SELECT ... FOR UPDATE, so the probe also held a row lock on a foreign record
// for the rest of the step — cross-tenant lock contention, and on a guessed row
// representation the ETag (an MD5 of the JSON body) confirms exact content.
//
// Asserting on the status alone would keep passing if the lock were reinstated
// ahead of the scope check and its result merely discarded. So count the calls.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// lockCountingAdapter forwards everything and counts the pessimistic reads,
// through the adapter and through a transaction alike — the DB step routes
// through whichever is live, and the guard under test opens its own transaction.
type lockCountingAdapter struct {
	maniflex.DBAdapter
	locks *atomic.Int64
}

func (a *lockCountingAdapter) FindByIDForUpdate(ctx context.Context, m *maniflex.ModelMeta, id string) (any, error) {
	a.locks.Add(1)
	return a.DBAdapter.FindByIDForUpdate(ctx, m, id)
}

func (a *lockCountingAdapter) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	tx, err := a.DBAdapter.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lockCountingTx{Tx: tx, locks: a.locks}, nil
}

type lockCountingTx struct {
	maniflex.Tx
	locks *atomic.Int64
}

func (t *lockCountingTx) FindByIDForUpdate(ctx context.Context, m *maniflex.ModelMeta, id string) (any, error) {
	t.locks.Add(1)
	return t.Tx.FindByIDForUpdate(ctx, m, id)
}

func lockCountSrv(t *testing.T) (*testutil.Server, *atomic.Int64) {
	t.Helper()
	locks := &atomic.Int64{}
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{ScopedAsset{}, maniflex.ModelConfig{OptimisticLock: true}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			return &lockCountingAdapter{DBAdapter: inner, locks: locks}, nil
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(orgScope(), maniflex.ForModel("ScopedAsset"))
		},
	})
	return srv, locks
}

func TestWriteGuards_IfMatchTakesNoLockOnAForeignRow(t *testing.T) {
	t.Parallel()
	srv, locks := lockCountSrv(t)

	id := srv.MustID(srv.POST("/scoped_assets",
		map[string]any{"name": "A's asset", "org_id": "tenant-a"}, asA))

	locks.Store(0)
	stale := map[string]string{"X-Org": "tenant-b", "If-Match": `"not-the-etag"`}
	srv.PATCH("/scoped_assets/"+id, map[string]any{"name": "X"}, stale).
		AssertStatus(http.StatusNotFound)

	if n := locks.Load(); n != 0 {
		t.Errorf("a cross-scope If-Match took %d FOR UPDATE lock(s) on the row; the "+
			"scope check must refuse it before any lock is taken", n)
	}
}

// The counter has to be able to observe a lock, or the assertion above is
// vacuous: the owner's own If-Match must still take one.
func TestWriteGuards_IfMatchStillLocksInScope(t *testing.T) {
	t.Parallel()
	srv, locks := lockCountSrv(t)

	id := srv.MustID(srv.POST("/scoped_assets",
		map[string]any{"name": "A's asset", "org_id": "tenant-a"}, asA))

	locks.Store(0)
	stale := map[string]string{"X-Org": "tenant-a", "If-Match": `"not-the-etag"`}
	srv.PATCH("/scoped_assets/"+id, map[string]any{"name": "X"}, stale).
		AssertStatus(http.StatusPreconditionFailed)

	if n := locks.Load(); n == 0 {
		t.Error("the owner's own If-Match took no lock; the counter cannot see the " +
			"call the test above asserts the absence of")
	}
}

// lock_scope has the same shape on create: the referenced row is located by a
// client-supplied id and locked. An out-of-scope target must be refused before
// that lock, not after.
func TestWriteGuards_LockScopeTakesNoLockOnAForeignRow(t *testing.T) {
	t.Parallel()

	locks := &atomic.Int64{}
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{ScopedBatch{}, ScopedDraw{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			return &lockCountingAdapter{DBAdapter: inner, locks: locks}, nil
		},
		Middleware: func(s *maniflex.Server) {
			for _, name := range []string{"ScopedBatch", "ScopedDraw"} {
				s.Pipeline.DB.Register(orgScope(), maniflex.ForModel(name))
			}
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("ScopedDraw"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	batch := srv.MustID(srv.POST("/scoped_batches",
		map[string]any{"label": "A's batch", "org_id": "tenant-a"}, asA))

	locks.Store(0)
	srv.POST("/scoped_draws", map[string]any{"batch_id": batch}, asB).
		AssertStatus(http.StatusNotFound)
	if n := locks.Load(); n != 0 {
		t.Errorf("a create naming another tenant's lock_scope target took %d lock(s)", n)
	}

	// And the in-scope create still takes exactly the lock it exists to take.
	locks.Store(0)
	srv.POST("/scoped_draws",
		map[string]any{"batch_id": batch, "org_id": "tenant-a"}, asA).
		AssertStatus(http.StatusCreated)
	if n := locks.Load(); n == 0 {
		t.Error("the in-scope create took no lock; lock_scope stopped doing its job")
	}
}
