package e2e

// WithTransaction cleared ctx.Tx once it committed, but left the same tx behind
// the txContextKey value in ctx.Ctx. A middleware that resumed after next() —
// an audit hook, a jobs/sql outbox enqueue — reached it via TxFromContext and
// wrote into a finished transaction, getting sql.ErrTxDone (BUG-14).

import (
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// txWatcher records what the transaction handles look like at two points: from
// inside the transaction (a DB-step middleware) and after WithTransaction has
// closed it (an Auth-step middleware resuming from next(), which unwinds last).
type txWatcher struct {
	mu       sync.Mutex
	duringTx maniflex.Tx // TxFromContext while the tx is open
	afterCtx maniflex.Tx // TxFromContext once the tx has closed
	afterTx  maniflex.Tx // ctx.Tx once the tx has closed
}

func (w *txWatcher) install(s *maniflex.Server, ops ...maniflex.Operation) {
	s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.afterTx = ctx.Tx
		w.afterCtx = maniflex.TxFromContext(ctx.Ctx)
		return nil
	}, maniflex.ForOperation(ops...))

	s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
		w.mu.Lock()
		w.duringTx = maniflex.TxFromContext(ctx.Ctx)
		w.mu.Unlock()
		return next()
	}, maniflex.ForOperation(ops...))

	s.Pipeline.Service.Register(maniflex.WithTransaction(nil), maniflex.ForOperation(ops...))
}

func (w *txWatcher) assertClosed(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.duringTx == nil {
		t.Error("TxFromContext returned nil inside the transaction — the outbox path can no longer join it")
	}
	if w.afterTx != nil {
		t.Error("ctx.Tx still holds the finished transaction")
	}
	if w.afterCtx != nil {
		t.Error("TxFromContext still returns the finished transaction; using it yields sql.ErrTxDone")
	}
}

func TestTxContext_ClearedAfterCommit(t *testing.T) {
	t.Parallel()
	w := &txWatcher{}
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) { w.install(s, maniflex.OpCreate) },
	})

	srv.CreateUser("Ada", "txctx1@x.com", "viewer").AssertStatus(http.StatusCreated)
	w.assertClosed(t)
}

func TestTxContext_ClearedAfterRollback(t *testing.T) {
	t.Parallel()
	w := &txWatcher{}
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			w.install(s, maniflex.OpCreate)
			// A post-insert rule that fails, so WithTransaction rolls back.
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if err := next(); err != nil {
					return err
				}
				ctx.Abort(http.StatusUnprocessableEntity, "POST_INSERT_FAIL", "business rule failed")
				return nil
			}, maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
		},
	})

	srv.CreateUser("Grace", "txctx2@x.com", "viewer").AssertStatus(http.StatusUnprocessableEntity)
	w.assertClosed(t)
}
