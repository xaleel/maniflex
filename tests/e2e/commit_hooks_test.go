package e2e_test

// Audit PIPE-3 — ctx.AfterCommit deferred only when WithTransaction had opened
// the transaction, because WithTransaction was the only thing that ever claimed
// the hook queue. Under maniflex.Batch, or under Execute carrying a caller-owned
// Tx, the hook fired inline *inside the still-open transaction* — and a later
// rollback could not take it back.
//
// That is EV-3, the race the framework already fixed for the HTTP path, reopened
// through two other doors: Batch owned a transaction without claiming the queue,
// and Execute builds a fresh ServerContext whose queue nobody drained. The only
// in-tree consumer, events.Emit, ignores the bool AfterCommit returns, so both
// were silent.
//
// The queue now belongs to whoever opened the transaction and is published on
// ctx.Ctx beside it, so a nested ServerContext appends to the owner's queue
// instead of firing into the void.
//
// A transaction opened by hand with ctx.BeginTx still runs hooks inline: the
// caller alone knows when their Commit succeeded. That is the documented
// fallback, and it now says so in the log — pinned by
// TestAfterCommit_HandRolledTxRunsInlineAndWarns.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// hookSrv registers events.Emit on the DB step and nothing else: the caller owns
// the transaction, which is the whole point of these paths.
func hookSrv(t *testing.T, bus events.Publisher, logger *slog.Logger) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{TxNote{}},
		Logger: logger,
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				events.Emit(bus),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After))
		},
	})
}

func hookBg(t *testing.T, srv *testutil.Server) *maniflex.ServerContext {
	t.Helper()
	mfx := srv.ManiflexServer()
	return maniflex.NewBackground(context.Background(), mfx.DB(), mfx.Registry())
}

// ── Batch owns the transaction, so it owns the queue ─────────────────────────

func TestBatch_AfterCommitHookIsDroppedOnRollback(t *testing.T) {
	srv := hookSrv(t, &plainBus{}, nil)
	bg := hookBg(t, srv)

	var deferred, ran bool
	err := maniflex.Batch(bg, func(b *maniflex.Batcher) error {
		if _, err := b.Create("TxNote", map[string]any{"title": "batched"}); err != nil {
			return err
		}
		deferred = bg.AfterCommit(func() { ran = true })
		return errors.New("a later item failed")
	})
	if err == nil {
		t.Fatal("precondition: the batch must fail so it rolls back")
	}
	if !deferred {
		t.Error("AfterCommit reported the hook ran inline; Batch owns the transaction and must queue it")
	}
	if ran {
		t.Error("the hook ran for a batch that rolled back")
	}
	if list := srv.GET("/tx_notes").DataList(); len(list) != 0 {
		t.Fatalf("precondition: %d row(s) survived the rollback; the fixture is wrong", len(list))
	}
}

// The other half: deferring must not become swallowing.
func TestBatch_AfterCommitHookRunsOnCommit(t *testing.T) {
	srv := hookSrv(t, &plainBus{}, nil)
	bg := hookBg(t, srv)

	var ran bool
	if err := maniflex.Batch(bg, func(b *maniflex.Batcher) error {
		if _, err := b.Create("TxNote", map[string]any{"title": "kept"}); err != nil {
			return err
		}
		bg.AfterCommit(func() { ran = true })
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !ran {
		t.Error("the hook never ran for a batch that committed")
	}
	if list := srv.GET("/tx_notes").DataList(); len(list) != 1 {
		t.Fatalf("precondition: %d row(s) after a committed batch, want 1", len(list))
	}
}

// A batch that joins an outer transaction claims nothing — that owner still
// decides. So a hook registered inside the batch must outlive the batch's return
// and fire only when the outer transaction commits.
func TestBatch_JoiningAnOuterTxLeavesTheQueueToItsOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		commit  bool
		wantRan bool
	}{
		{"outer commits", true, true},
		{"outer rolls back", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := &plainBus{}
			var ran bool
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{TxNote{}},
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.Service.Register(
						maniflex.WithTransaction(nil),
						maniflex.ForOperation(maniflex.OpCreate))
					s.Pipeline.DB.Register(events.Emit(bus),
						maniflex.ForOperation(maniflex.OpCreate),
						maniflex.AtPosition(maniflex.After))
					s.Pipeline.Service.Register(
						func(ctx *maniflex.ServerContext, next func() error) error {
							if err := next(); err != nil {
								return err
							}
							if err := maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
								ctx.AfterCommit(func() { ran = true })
								return nil
							}); err != nil {
								return err
							}
							if !tc.commit {
								ctx.Abort(500, "BOOM", "after the batch, before the commit")
							}
							return nil
						},
						maniflex.ForOperation(maniflex.OpCreate),
						maniflex.AtPosition(maniflex.After))
				},
			})

			srv.POST("/tx_notes", map[string]any{"title": "joined"})
			if ran != tc.wantRan {
				t.Errorf("hook ran = %v, want %v — the inner batch must not decide "+
					"an outer transaction's hooks", ran, tc.wantRan)
			}
		})
	}
}

// ── Execute carries the caller's transaction, not their ServerContext ────────

// The headline: the documented multi-invocation loop, where one failure must
// roll the whole thing back. Every event it emitted has to go with it.
func TestExecute_RolledBackInvocationEmitsNoEvent(t *testing.T) {
	bus := &plainBus{}
	srv := hookSrv(t, bus, nil)
	mfx := srv.ManiflexServer()
	bg := hookBg(t, srv)

	err := maniflex.Batch(bg, func(b *maniflex.Batcher) error {
		if _, err := mfx.Execute(bg.Ctx, maniflex.Invocation{
			Model: "TxNote", Operation: maniflex.OpCreate,
			Body: map[string]any{"title": "phantom"}, Tx: bg.Tx,
		}); err != nil {
			return err
		}
		return errors.New("a later invocation failed")
	})
	if err == nil {
		t.Fatal("precondition: the unit must fail so it rolls back")
	}

	if n := bus.settle(1); n != 0 {
		t.Errorf("published %d event(s) for an Execute that rolled back: subscribers "+
			"now believe a record exists that does not", n)
	}
	if list := srv.GET("/tx_notes").DataList(); len(list) != 0 {
		t.Fatalf("precondition: %d row(s) survived the rollback; the fixture is wrong", len(list))
	}
}

func TestExecute_CommittedInvocationStillEmits(t *testing.T) {
	bus := &plainBus{}
	srv := hookSrv(t, bus, nil)
	mfx := srv.ManiflexServer()
	bg := hookBg(t, srv)

	if err := maniflex.Batch(bg, func(b *maniflex.Batcher) error {
		_, err := mfx.Execute(bg.Ctx, maniflex.Invocation{
			Model: "TxNote", Operation: maniflex.OpCreate,
			Body: map[string]any{"title": "kept"}, Tx: bg.Tx,
		})
		return err
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if n := bus.settle(1); n != 1 {
		t.Errorf("published %d event(s) after a successful commit, want 1", n)
	}
}

// The guard that keeps the ctx.Ctx lookup honest — a context carrying one
// transaction's queue while the ServerContext holds another — is a unit test in
// package maniflex (TestAfterCommit_ForeignTransactionIsNotQueued). Staging it
// here would mean two write transactions open at once, which SQLite does not
// allow.

// ── The documented fallback ──────────────────────────────────────────────────

// handRolledHook registers an after-commit hook from the Service step, inside a
// transaction it opened itself when withTx — no WithTransaction, no Batch, so
// nothing has promised to drain the queue.
func handRolledHook(deferred, ran *bool, withTx bool) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if withTx {
			tx, err := ctx.BeginTx(ctx.Ctx, nil)
			if err != nil {
				return err
			}
			ctx.Tx = tx
			defer func() {
				_ = tx.Rollback()
				ctx.Tx = nil
			}()
		}
		*deferred = ctx.AfterCommit(func() { *ran = true })
		return nil
	}
}

// A transaction opened by hand is not something the framework can drain: only
// the caller knows when their Commit returned. The hook runs inline, as
// AfterCommit documents — but no longer silently, because an inline hook inside
// an open transaction is exactly the race deferring exists to prevent.
//
// The quiet case is the other half, and it is the common one: with no
// transaction open, running inline is simply correct and must not warn.
func TestAfterCommit_InlineFallbackWarnsOnlyInsideAnOpenTx(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withTx   bool
		wantWarn bool
	}{
		{"hand-rolled transaction warns", true, true},
		{"no transaction is quiet", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf lockedBuffer
			var deferred, ran bool
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{TxNote{}},
				Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.Service.Register(
						handRolledHook(&deferred, &ran, tc.withTx),
						maniflex.ForOperation(maniflex.OpCreate),
						maniflex.AtPosition(maniflex.After))
				},
			})

			srv.POST("/tx_notes", map[string]any{"title": "hand-rolled"})

			if deferred {
				t.Error("nothing promised to drain the queue; the hook must run inline")
			}
			if !ran {
				t.Error("the hook was neither deferred nor run — it was lost")
			}
			warned := strings.Contains(buf.String(), "after-commit hook ran inline")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log: %s", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

// lockedBuffer is a bytes.Buffer safe for a handler writing from another
// goroutine, which the background publish path does.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
