package maniflex

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// txContextKey is the unexported context key used to store the active Tx.
type txContextKey struct{}

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
// WithTransaction is the middleware that drains the queue.
func (c *ServerContext) AfterCommit(fn func()) bool {
	if fn == nil {
		return false
	}
	if c.Tx == nil || !c.commitDrainer {
		fn()
		return false
	}
	c.commitHooks = append(c.commitHooks, fn)
	return true
}

// runCommitHooks fires the queued hooks in registration order and clears the
// queue, so a second transaction in the same request starts clean.
func (c *ServerContext) runCommitHooks() {
	hooks := c.commitHooks
	c.commitHooks = nil
	for _, fn := range hooks {
		fn()
	}
}

// dropCommitHooks discards the queue after a rollback. The count is logged
// rather than the hooks being run: their whole purpose is not to fire for a
// write that did not happen, but silence would make a dropped publish
// indistinguishable from one that was never registered.
func (c *ServerContext) dropCommitHooks() {
	if len(c.commitHooks) == 0 {
		return
	}
	c.Logger().Debug("transaction rolled back; after-commit hooks dropped",
		slog.Int("hooks", len(c.commitHooks)))
	c.commitHooks = nil
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
		// is in force, so AfterCommit defers instead of running inline. Restore
		// the previous value rather than clearing it: an outer WithTransaction
		// that already owns the queue must keep owning it.
		prevDrainer := ctx.commitDrainer
		ctx.commitDrainer = true

		// Rollback is always deferred. After Commit it becomes a no-op.
		defer func() {
			// Rollback after a successful commit returns sql.ErrTxDone
			// from database/sql, which we silently discard.
			_ = tx.Rollback()

			// Anything still queued belongs to a transaction that did not
			// commit — every commit path below drains the queue itself.
			ctx.dropCommitHooks()
			ctx.commitDrainer = prevDrainer

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
