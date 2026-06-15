package maniflex

import (
	"context"
	"fmt"
	"net/http"
)

// txContextKey is the unexported context key used to store the active Tx.
type txContextKey struct{}

// TxFromContext returns the active database transaction stored in ctx by
// WithTransaction, or nil when no transaction is active. Use this from
// packages (e.g. jobs/sql) that need to join the caller's transaction without
// holding a *ServerContext reference.
func TxFromContext(ctx context.Context) Tx {
	tx, _ := ctx.Value(txContextKey{}).(Tx)
	return tx
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

		// Rollback is always deferred. After Commit it becomes a no-op.
		defer func() {
			// Rollback after a successful commit returns sql.ErrTxDone
			// from database/sql, which we silently discard.
			_ = tx.Rollback()
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

		// Clear ctx.Tx so post-commit After middleware use the bare adapter.
		ctx.Tx = nil
		return nil
	}
}
