//go:build !nomaniflex_glue

// maniflex_glue.go wires maniflex.TxFromContext into the jobs/sql outbox path.
// When a Server request runs inside a maniflex.WithTransaction middleware, any
// Queue.Enqueue call that carries ctx will INSERT the job row through the
// same *sql.Tx, giving atomic outbox semantics for free.
//
// Binaries that use jobs/sql without maniflex (pure queue workers, CLI tools)
// can exclude this file with -tags nomaniflex_glue, which removes the compile-time
// dependency on the Server package entirely.
package sql

import (
	"context"

	"maniflex"
)

func init() {
	txFromContext = func(ctx context.Context) sqlExecer {
		tx := maniflex.TxFromContext(ctx)
		if tx == nil {
			return nil
		}
		// sqlcore.txAdapter exposes ExecContext; check for it.
		if exec, ok := tx.(sqlExecer); ok {
			return exec
		}
		return nil
	}
}
