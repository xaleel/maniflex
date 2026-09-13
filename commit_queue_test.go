package maniflex

// Audit PIPE-3 — the after-commit queue belongs to whoever opened the
// transaction and is published on ctx.Ctx beside it, so a ServerContext running
// inside a transaction it did not open (Execute builds one) can still find it.
//
// That lookup is keyed on the context, and a context can outlive or out-scope
// the transaction it was carrying. These pin the two halves of the rule: the
// queue is used when the context's transaction is the one this ServerContext
// holds, and ignored when it is not.

import (
	"context"
	"testing"
)

// bareTx is a Tx that does nothing. Nothing here reaches the database — the
// question is only which queue a hook lands in.
type bareTx struct{ name string }

func (bareTx) FindByID(context.Context, *ModelMeta, string, *QueryParams) (any, error) {
	return nil, nil
}

func (bareTx) FindMany(context.Context, *ModelMeta, *QueryParams) ([]any, int64, error) {
	return nil, 0, nil
}
func (bareTx) Create(context.Context, *ModelMeta, any) (any, error) { return nil, nil }

func (bareTx) Update(context.Context, *ModelMeta, string, any, map[string]struct{}) (any, error) {
	return nil, nil
}
func (bareTx) Delete(context.Context, *ModelMeta, string) error { return nil }

func (bareTx) FindByIDForUpdate(context.Context, *ModelMeta, string) (any, error) {
	return nil, nil
}
func (bareTx) Commit() error   { return nil }
func (bareTx) Rollback() error { return nil }

// nested builds the shape Execute produces: a fresh ServerContext with no queue
// of its own, holding tx, whose Ctx descends from the owner's.
func nested(ownerCtx context.Context, tx Tx) *ServerContext {
	return &ServerContext{Ctx: ownerCtx, Tx: tx}
}

// ownerCtx is a ServerContext that has claimed the queue for tx, as
// WithTransaction and Batch do.
func ownerCtx(tx Tx) (*ServerContext, func()) {
	c := &ServerContext{Ctx: context.Background(), Tx: tx}
	c.Ctx = context.WithValue(c.Ctx, txContextKey{}, tx)
	release := c.ownCommitQueue()
	return c, release
}

func TestAfterCommit_NestedContextFindsTheOwnersQueue(t *testing.T) {
	tx := bareTx{"owner"}
	owner, release := ownerCtx(tx)
	defer release()

	var ran bool
	child := nested(owner.Ctx, tx)
	if deferred := child.AfterCommit(func() { ran = true }); !deferred {
		t.Fatal("a context holding the owner's transaction must queue onto the owner's queue")
	}
	if ran {
		t.Fatal("the hook ran immediately; it was supposed to wait for the commit")
	}

	owner.runCommitHooks()
	if !ran {
		t.Error("the owner's commit did not run a hook queued from a nested context")
	}
}

func TestAfterCommit_NestedContextHookIsDroppedWithTheOwnersRollback(t *testing.T) {
	tx := bareTx{"owner"}
	owner, release := ownerCtx(tx)

	var ran bool
	nested(owner.Ctx, tx).AfterCommit(func() { ran = true })

	release() // what the owner's deferred rollback path does
	if ran {
		t.Error("a hook queued from a nested context survived the owner's rollback")
	}
}

// The guard: the queue on the context belongs to the owner's transaction, not to
// whichever one this ServerContext happens to hold. Queuing here would fire the
// hook on a commit that says nothing about the transaction the work went into —
// and that transaction may yet roll back.
func TestAfterCommit_ForeignTransactionIsNotQueued(t *testing.T) {
	owner, release := ownerCtx(bareTx{"owner"})
	defer release()

	var ran bool
	child := nested(owner.Ctx, bareTx{"foreign"})
	if deferred := child.AfterCommit(func() { ran = true }); deferred {
		t.Error("a hook for a foreign transaction was queued onto the owner's queue")
	}
	if !ran {
		t.Error("the hook was neither queued nor run — it was lost")
	}

	// And the owner's queue is untouched, so its own commit has nothing to fire.
	ran = false
	owner.runCommitHooks()
	if ran {
		t.Error("the foreign hook was sitting in the owner's queue after all")
	}
}

// A second transaction in the same request starts clean: hooks are taken from
// the queue, not merely read, so a drained hook cannot run twice.
func TestCommitQueue_DrainIsOnce(t *testing.T) {
	owner, release := ownerCtx(bareTx{"owner"})
	defer release()

	runs := 0
	owner.AfterCommit(func() { runs++ })
	owner.runCommitHooks()
	owner.runCommitHooks()

	if runs != 1 {
		t.Errorf("hook ran %d times across two drains, want 1", runs)
	}
}
