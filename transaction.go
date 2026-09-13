package maniflex

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
)

// txContextKey is the unexported context key used to store the active Tx.
type txContextKey struct{}

// commitQueueKey is the unexported context key carrying the active after-commit
// queue, stored beside the transaction it belongs to.
type commitQueueKey struct{}

// commitQueue holds the after-commit hooks for one transaction.
//
// It is owned by whatever opened that transaction — WithTransaction, Batch — and
// published on ctx.Ctx beside the transaction itself, which is what lets a
// ServerContext that did *not* open the transaction still reach it. Execute
// builds a fresh ServerContext and copies the caller's Tx onto it; without a
// shared queue its hooks had nowhere to go but inline, inside a transaction
// somebody else was still deciding whether to commit (audit PIPE-3).
//
// The mutex is there because that sharing makes concurrent registration
// possible: two Execute calls against one parent transaction append to the same
// queue. The hooks themselves still run one at a time, in registration order, on
// whichever goroutine commits.
type commitQueue struct {
	mu    sync.Mutex
	hooks []func()
}

func (q *commitQueue) add(fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.hooks = append(q.hooks, fn)
}

// take empties the queue and returns what it held, so a second transaction in
// the same request starts clean and a hook cannot be run twice.
func (q *commitQueue) take() []func() {
	q.mu.Lock()
	defer q.mu.Unlock()
	hooks := q.hooks
	q.hooks = nil
	return hooks
}

// commitQueueFrom returns the queue published on ctx, or nil when none is. A
// cleared value reads back as a typed nil pointer, which compares nil as
// intended.
func commitQueueFrom(ctx context.Context) *commitQueue {
	q, _ := ctx.Value(commitQueueKey{}).(*commitQueue)
	return q
}

// TxFromContext returns the active database transaction stored in ctx by
// WithTransaction, or nil when no transaction is active. Use this from
// packages (e.g. jobs/sql) that need to join the caller's transaction without
// holding a *ServerContext reference.
//
// It returns nil once WithTransaction has committed or rolled back, so a caller
// that resumes after the transaction closed gets the bare adapter rather than a
// finished transaction.
func TxFromContext(ctx context.Context) Tx {
	tx, _ := ctx.Value(txContextKey{}).(Tx)
	return tx
}

// AfterCommit registers fn to run once the request's transaction has committed
// successfully. If the transaction rolls back, fn is never called.
//
// Use it for side effects that must not become visible before the write they
// describe is durable — publishing to a broker, enqueuing a job, calling a
// webhook. Without it, a side effect fired from inside the transaction is
// announcing a write that may still be rolled back, and a subscriber that reads
// the record straight back races the commit and finds nothing (audit EV-3).
//
// fn runs synchronously, after the commit and before the request returns; keep
// it short, or have it start its own goroutine.
//
// AfterCommit reports whether fn was deferred. It returns false — having already
// called fn inline — when there is no transaction to wait for, or when the
// transaction was opened by something that does not run commit hooks. Deferring
// into a queue nobody drains would lose the side effect entirely, which is worse
// than performing it early, so the fallback is to run it. Callers that only need
// the side effect to happen can ignore the result:
//
//	ctx.AfterCommit(func() { bus.Publish(bgCtx, e) })
//
// WithTransaction and Batch own a transaction and drain the queue. A transaction
// you opened yourself with ctx.BeginTx does not: you call Commit, so only you
// know when it succeeded, and the inline fallback applies — noisily, since a
// hook that ran inside a transaction that then rolled back is the very race
// deferring exists to prevent.
func (c *ServerContext) AfterCommit(fn func()) bool {
	if fn == nil {
		return false
	}
	if c.Tx != nil {
		// This context's own queue, or — for one that is running inside a
		// transaction it did not open, which is what Execute builds — the
		// owner's, published on ctx.Ctx beside that transaction.
		//
		// The Tx comparison is what keeps that second case honest: a caller can
		// pass the owning request's context while handing Execute a *different*
		// transaction, and queuing the hook there would fire it on the wrong
		// commit — for a write that may have rolled back with the transaction it
		// actually belonged to.
		q := c.commitQueue
		if q == nil && TxFromContext(c.Ctx) == c.Tx {
			q = commitQueueFrom(c.Ctx)
		}
		if q != nil {
			q.add(fn)
			return true
		}
		c.Logger().Warn("after-commit hook ran inline inside an open transaction; a rollback cannot take it back",
			slog.String("hint", "open the transaction with maniflex.WithTransaction or maniflex.Batch, "+
				"or perform the side effect after your own Commit returns"))
	}
	fn()
	return false
}

// runCommitHooks fires the queued hooks in registration order and empties the
// queue, so a second transaction in the same request starts clean.
func (c *ServerContext) runCommitHooks() {
	if c.commitQueue == nil {
		return
	}
	for _, fn := range c.commitQueue.take() {
		fn()
	}
}

// dropCommitHooks discards the queue after a rollback. The count is logged
// rather than the hooks being run: their whole purpose is not to fire for a
// write that did not happen, but silence would make a dropped publish
// indistinguishable from one that was never registered.
func (c *ServerContext) dropCommitHooks() {
	if c.commitQueue == nil {
		return
	}
	if dropped := len(c.commitQueue.take()); dropped > 0 {
		c.Logger().Debug("transaction rolled back; after-commit hooks dropped",
			slog.Int("hooks", dropped))
	}
}

// ownCommitQueue claims the after-commit queue for a transaction this caller
// owns, publishing it on ctx.Ctx — beside the transaction handle, which the
// caller puts there — so a nested ServerContext can find it. It returns the
// restore function the owner must defer.
//
// The previous queue is restored rather than cleared: an outer owner that
// already holds one must keep holding it.
func (c *ServerContext) ownCommitQueue() func() {
	prev := c.commitQueue
	c.commitQueue = &commitQueue{}
	c.Ctx = context.WithValue(c.Ctx, commitQueueKey{}, c.commitQueue)
	return func() {
		// Anything still queued belongs to a transaction that did not commit —
		// every commit path drains the queue itself before this runs.
		c.dropCommitHooks()
		c.commitQueue = prev
		// Overwritten rather than restored, so whatever the downstream steps
		// added to ctx.Ctx survives — the same thing WithTransaction does with
		// the transaction handle.
		c.Ctx = context.WithValue(c.Ctx, commitQueueKey{}, prev)
	}
}

// WithTransaction wraps the pipeline's DB step in a database transaction.
// It begins a transaction before the DB step runs and commits it after all
// After-DB middleware complete. If any step returns an error or sets an error
// response, the transaction is rolled back instead.
//
// Register it on the Service step (Before position, the default) so it fires
// just before the DB step:
//
//	server.Pipeline.Service.Register(
//	    maniflex.WithTransaction(nil), // nil opts = default isolation
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	)
//
// Or on the DB step itself at Replace position to fully replace the default:
//
//	server.Pipeline.DB.Register(
//	    maniflex.WithTransaction(nil),
//	    maniflex.AtPosition(maniflex.Replace),
//	)
//
// Once registered, middleware running in the same request can read ctx.Tx to
// join the same transaction, or call ctx.BeginTx themselves for nested work.
//
// WithTransaction is safe to use with both Postgres and SQLite. SQLite does not
// support nested transactions; registering WithTransaction twice for the same
// request will return an error from the second BeginTx call.
func WithTransaction(opts *TxOptions) MiddlewareFunc {
	return func(ctx *ServerContext, next func() error) error {
		// If a transaction is already active (e.g. set by an outer middleware),
		// do not start another one — just continue in the existing transaction.
		if ctx.Tx != nil {
			return next()
		}

		tx, err := ctx.BeginTx(ctx.Ctx, opts)
		if err != nil {
			ctx.Abort(http.StatusInternalServerError, "TX_BEGIN_ERROR",
				fmt.Sprintf("failed to begin transaction: %v", err))
			return nil
		}

		// Take responsibility for the after-commit queue while this transaction
		// is in force, so AfterCommit defers instead of running inline.
		releaseQueue := ctx.ownCommitQueue()

		// Rollback is always deferred. After Commit it becomes a no-op.
		defer func() {
			// Rollback after a successful commit returns sql.ErrTxDone
			// from database/sql, which we silently discard.
			_ = tx.Rollback()

			releaseQueue()

			// The transaction is finished either way by the time this returns.
			// Clear both handles on it — ctx.Tx and the context value — so a
			// middleware that resumes after next() (an audit hook, an outbox
			// enqueue reaching for TxFromContext) falls back to the bare adapter
			// instead of writing into a committed or rolled-back tx and getting
			// sql.ErrTxDone. Overwriting the value keeps anything the downstream
			// steps added to ctx.Ctx.
			ctx.Tx = nil
			ctx.Ctx = context.WithValue(ctx.Ctx, txContextKey{}, Tx(nil))
		}()

		// Expose the transaction to all subsequent steps and middleware,
		// and store it in ctx.Ctx so packages without a *ServerContext reference
		// (e.g. jobs/sql outbox enqueue) can reach it via TxFromContext.
		ctx.Tx = tx
		ctx.Ctx = context.WithValue(ctx.Ctx, txContextKey{}, tx)

		// Run the rest of the pipeline (DB step + After middleware).
		if err := next(); err != nil {
			// Pipeline error — rollback happens via the deferred call above.
			return err
		}

		// If any step set an error response, rollback instead of committing.
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			// Rollback via deferred call; nothing to do here.
			return nil
		}

		// All steps succeeded — commit the transaction.
		if err := tx.Commit(); err != nil {
			ctx.Abort(http.StatusInternalServerError, "TX_COMMIT_ERROR",
				fmt.Sprintf("failed to commit transaction: %v", err))
			return nil
		}

		// Durable now, so the side effects that were waiting on it may fire.
		// A commit that failed above falls through to the deferred drop: the
		// write did not happen and neither may its announcements.
		ctx.runCommitHooks()
		return nil
	}
}
