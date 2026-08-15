package maniflex

// H3 — the rollup recompute was a SELECT-then-UPDATE inside the request's
// transaction. On Postgres (read committed) two concurrent child writes for the
// same parent each aggregated without seeing the other's uncommitted row; the
// second UPDATE blocked on the parent row lock and then overwrote the first with
// its own stale total. Permanent, silent drift in exactly the counter a rollup
// exists to keep correct.
//
// The fix is to take the parent's row lock *before* aggregating, so the rival
// transaction has to commit first and its rows are counted. What matters is the
// ordering, which is what these tests pin: the race itself needs Postgres and
// two live connections, but a lock taken after the aggregate is useless on any
// driver and is catchable here.
//
// SQLite is not affected either way — db/sqlite opens write connections with
// _txlock=immediate, so a second read-then-write transaction waits at BEGIN.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recordingTx records the order of the operations a rollup performs. Both the
// aggregate (ServerContext.rawQuery) and the parent write route through ctx.Tx,
// so one spy sees the whole sequence.
type recordingTx struct {
	mu  sync.Mutex
	ops []string

	// childRow is what a pre-write read of the child returns, which is where
	// affectedParents learns the parent a re-parenting update is moving away from.
	childRow *RollupChildT

	// lockErr, when set, is what FindByIDForUpdate returns — a parent that is
	// gone or soft-deleted answers ErrNotFound.
	lockErr error
}

func (t *recordingTx) record(op string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ops = append(t.ops, op)
}

func (t *recordingTx) calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.ops...)
}

// ── Tx ────────────────────────────────────────────────────────────────────────

func (t *recordingTx) FindByID(_ context.Context, m *ModelMeta, id string, _ *QueryParams) (any, error) {
	t.record("read:" + m.Name + ":" + id)
	if t.childRow != nil && m.Name == "RollupChildT" {
		return t.childRow, nil
	}
	return nil, nil
}

func (t *recordingTx) FindMany(context.Context, *ModelMeta, *QueryParams) ([]any, int64, error) {
	return nil, 0, nil
}

func (t *recordingTx) Create(context.Context, *ModelMeta, any) (any, error) { return nil, nil }

func (t *recordingTx) Update(_ context.Context, m *ModelMeta, id string, _ any, _ map[string]struct{}) (any, error) {
	t.record("update:" + m.Name + ":" + id)
	return nil, nil
}

func (t *recordingTx) Delete(context.Context, *ModelMeta, string) error { return nil }

func (t *recordingTx) FindByIDForUpdate(_ context.Context, m *ModelMeta, id string) (any, error) {
	t.record("lock:" + m.Name + ":" + id)
	return nil, t.lockErr
}

func (t *recordingTx) Commit() error   { return nil }
func (t *recordingTx) Rollback() error { return nil }

// ── rawableT ──────────────────────────────────────────────────────────────────

func (t *recordingTx) RawQueryContext(_ context.Context, _ string, _ ...any) ([]map[string]any, error) {
	t.record("aggregate")
	return []map[string]any{{"v": int64(150)}}, nil
}

func (t *recordingTx) RawExecContext(context.Context, string, ...any) (int64, error) {
	return 0, nil
}

func indexOf(ops []string, want string) int {
	for i, op := range ops {
		if op == want {
			return i
		}
	}
	return -1
}

// The lock has to come first. Taking it after the aggregate would leave the
// window the race lives in: the rival commits in between, and its rows are
// still missing from the total that gets written.
func TestRollupRecompute_LocksParentBeforeAggregating(t *testing.T) {
	srv := rollupTestServer(t)
	cr, err := srv.compileRollup(validRollup())
	if err != nil {
		t.Fatalf("compileRollup: %v", err)
	}

	tx := &recordingTx{}
	ctx := &ServerContext{Ctx: context.Background(), reg: srv.registry, Tx: tx}

	if err := cr.recompute(ctx, "parent-1"); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	ops := tx.calls()
	lockAt := indexOf(ops, "lock:RollupParentT:parent-1")
	aggAt := indexOf(ops, "aggregate")

	if lockAt < 0 {
		t.Fatalf("recompute never locked the parent row; ops: %v\n"+
			"Without it, two concurrent child writes each aggregate without seeing the "+
			"other's uncommitted row and the second UPDATE overwrites the first with a "+
			"stale total.", ops)
	}
	if aggAt < 0 {
		t.Fatalf("recompute never ran the aggregate; ops: %v", ops)
	}
	if lockAt > aggAt {
		t.Fatalf("parent locked AFTER the aggregate; ops: %v\n"+
			"The lock must precede the aggregate — otherwise the rival transaction "+
			"commits in the gap and its rows are still missed.", ops)
	}
}

// A parent that cannot be locked — gone, or soft-deleted, since the lock query
// is soft-delete scoped — must fail the write rather than quietly skipping the
// recompute, which would leave the drift the rollup exists to prevent. The
// parent write was already soft-delete scoped and answered ErrNotFound for the
// same rows, so this reports the same failure one statement earlier.
func TestRollupRecompute_UnlockableParentFailsClosed(t *testing.T) {
	srv := rollupTestServer(t)
	cr, err := srv.compileRollup(validRollup())
	if err != nil {
		t.Fatalf("compileRollup: %v", err)
	}

	tx := &recordingTx{lockErr: ErrNotFound}
	ctx := &ServerContext{Ctx: context.Background(), reg: srv.registry, Tx: tx}

	if err := cr.recompute(ctx, "gone"); err == nil {
		t.Fatal("recompute returned nil for a parent it could not lock — the rollup " +
			"would silently not be recomputed")
	}
	for _, op := range tx.calls() {
		if op == "aggregate" || strings.HasPrefix(op, "update:") {
			t.Fatalf("recompute continued past a failed lock (ops: %v); the aggregate "+
				"and the write must not run once the parent could not be locked", tx.calls())
		}
	}
}

// Locking introduces a hazard of its own: a write touching two parents (a
// re-parenting update recomputes both) must take their locks in an order every
// transaction agrees on, or two of them racing in opposite directions deadlock.
// affectedParents returns a map, whose iteration order Go deliberately
// randomises, so the order has to be imposed.
func TestRollupMiddleware_LocksParentsInDeterministicOrder(t *testing.T) {
	srv := rollupTestServer(t)
	cr, err := srv.compileRollup(validRollup())
	if err != nil {
		t.Fatalf("compileRollup: %v", err)
	}

	// Run it repeatedly: with map order alone, a fixed expectation would pass by
	// luck roughly half the time.
	for range 20 {
		tx := &recordingTx{childRow: &RollupChildT{ParentID: "zzz-old-parent"}}
		ctx := &ServerContext{
			Ctx:        context.Background(),
			reg:        srv.registry,
			Tx:         tx,
			Operation:  OpUpdate,
			ResourceID: "child-1",
			ParsedBody: NewRequestBody(map[string]any{"parent_id": "aaa-new-parent"}),
		}

		if err := cr.middleware()(ctx, func() error { return nil }); err != nil {
			t.Fatalf("rollup middleware: %v", err)
		}

		var locked []string
		for _, op := range tx.calls() {
			if id, ok := strings.CutPrefix(op, "lock:RollupParentT:"); ok {
				locked = append(locked, id)
			}
		}

		want := []string{"aaa-new-parent", "zzz-old-parent"}
		if len(locked) != len(want) {
			t.Fatalf("locked %v, want both parents %v", locked, want)
		}
		for i := range want {
			if locked[i] != want[i] {
				t.Fatalf("parents locked in order %v, want %v — a write touching two "+
					"parents must lock them in an order every transaction agrees on, or "+
					"two racing in opposite directions deadlock", locked, want)
			}
		}
	}
}
