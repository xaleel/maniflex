package maniflex

import (
	"context"
	"fmt"
)

// Batcher provides transactional CRUD access to registered models inside a
// maniflex.Batch call. Do not construct directly — obtain via maniflex.Batch.
type Batcher struct {
	ctx     *ServerContext
	exec    dbExec
	adapter DBAdapter // the adapter the batch transaction was opened on
}

// Model returns a *ModelAccessor for modelName bound to the batch transaction.
// All five CRUD operations on the accessor participate in the same transaction.
// The accessor records an error on first use when modelName is not registered,
// or when modelName routes to a different adapter than the batch transaction
// (a single transaction cannot span databases).
func (b *Batcher) Model(name string) *ModelAccessor {
	if b.ctx.reg == nil {
		return &ModelAccessor{err: fmt.Errorf("maniflex: registry not available")}
	}
	meta, ok := b.ctx.reg.Get(name)
	if !ok {
		return &ModelAccessor{err: fmt.Errorf("maniflex: model %q is not registered", name)}
	}
	if target := meta.ResolveAdapter(b.ctx.adapter); target != b.adapter {
		return &ModelAccessor{err: fmt.Errorf(
			"maniflex: Batch: model %q routes to a different adapter than the batch transaction; "+
				"use pkg/saga for cross-adapter coordination", name)}
	}
	return &ModelAccessor{meta: meta, exec: b.exec, ctx: b.ctx.Ctx}
}

// Create inserts a new record for model and returns the stored representation.
func (b *Batcher) Create(model string, data map[string]any) (map[string]any, error) {
	return b.Model(model).Create(data)
}

// Read returns the record identified by id from model.
// Returns maniflex.ErrNotFound when the record does not exist.
func (b *Batcher) Read(model string, id string) (map[string]any, error) {
	return b.Model(model).Read(id)
}

// Update applies a partial patch to the record identified by id in model and
// returns the updated representation. Returns maniflex.ErrNotFound when absent.
func (b *Batcher) Update(model string, id string, data map[string]any) (map[string]any, error) {
	return b.Model(model).Update(id, data)
}

// Delete removes (or soft-deletes) the record identified by id from model.
// Returns maniflex.ErrNotFound when absent.
func (b *Batcher) Delete(model string, id string) error {
	return b.Model(model).Delete(id)
}

// List returns a page of records from model matching q.
// q may be nil (defaults to page 1, limit 20, no filters or sorts).
func (b *Batcher) List(model string, q *QueryParams) ([]map[string]any, error) {
	return b.Model(model).List(q)
}

// Batch executes fn inside a single database transaction shared by all Batcher
// operations. On success the transaction commits; on any error it rolls back.
//
// Transaction ownership:
//   - If ctx.Tx is nil, Batch opens a new transaction, commits on success,
//     rolls back on fn error or ctx.Abort, and restores ctx.Tx to nil on exit.
//   - If ctx.Tx is already set (e.g. from WithTransaction or a manual BeginTx),
//     Batch joins it. It does not commit or roll back — the outer owner is
//     responsible for lifecycle. fn returning an error still propagates up so
//     the outer owner can react.
//
// ctx.Tx and ctx.Ctx are updated to point at the batch transaction for the
// duration of fn, so any code inside fn that calls ctx.RawQuery,
// ctx.LockForUpdate, or ctx.GetModel also participates in the same transaction.
// Both fields are restored when Batch returns.
//
//	err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
//	    inv, err := b.Create("Invoice", invoiceData)
//	    if err != nil { return err }
//	    for _, line := range lines {
//	        line["invoice_id"] = inv["id"]
//	        if _, err := b.Create("InvoiceLine", line); err != nil { return err }
//	    }
//	    return nil
//	})
func Batch(ctx *ServerContext, fn func(*Batcher) error) error {
	adapter := ctx.requestAdapter()
	if adapter == nil {
		return ErrNoAdapter
	}
	if ctx.reg == nil {
		return fmt.Errorf("maniflex: registry not available on this ServerContext")
	}

	ownsTransaction := ctx.Tx == nil
	tx := ctx.Tx

	if ownsTransaction {
		var err error
		tx, err = ctx.BeginTx(ctx.Ctx, nil)
		if err != nil {
			return fmt.Errorf("maniflex: batch: begin tx: %w", err)
		}
		prevTx := ctx.Tx
		prevCtx := ctx.Ctx
		ctx.Tx = tx
		ctx.Ctx = context.WithValue(ctx.Ctx, txContextKey{}, tx)
		defer func() {
			_ = tx.Rollback() // no-op after a successful Commit
			ctx.Tx = prevTx
			ctx.Ctx = prevCtx
		}()
	}

	b := &Batcher{ctx: ctx, exec: dbExec{adapter: adapter, tx: tx}, adapter: adapter}

	if err := fn(b); err != nil {
		return err
	}

	// ctx.Abort sets ctx.Response with a ≥400 status and returns nil from the
	// fn. Without this check Batch would commit despite the abort. Mirrors the
	// same guard in WithTransaction.
	if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
		if ctx.Response.Error != nil {
			return fmt.Errorf("maniflex: batch aborted: %s", ctx.Response.Error.Code)
		}
		return fmt.Errorf("maniflex: batch aborted with status %d", ctx.Response.StatusCode)
	}

	if ownsTransaction {
		return tx.Commit()
	}
	return nil
}
