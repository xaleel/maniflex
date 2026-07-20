package e2e_test

// Audit EV-3 (High): the transactional-outbox branch in events.Emit fires only
// when the Publisher implements TxPublisher AND ctx.Tx is non-nil. With a direct
// broker bus (redis/kafka/nats/rabbitmq) under WithTransaction the assertion
// fails, so Emit falls through to its fire-and-forget goroutine — which
// publishes on context.Background() *before* the transaction commits.
//
// If the transaction then rolls back, subscribers have already seen an event for
// a write that never happened.
//
//	go test ./tests/e2e/... -run TestEmitTx

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TxNote is an ordinary model; nothing about EV-3 depends on its shape.
type TxNote struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

// plainBus is a direct broker bus: a Publisher and nothing more. It deliberately
// does NOT implement events.TxPublisher, which is the whole point — that is what
// every real broker adapter looks like.
type plainBus struct {
	mu  sync.Mutex
	got []events.Event
}

func (b *plainBus) Publish(_ context.Context, e events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, e)
	return nil
}

func (b *plainBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (b *plainBus) Close() error { return nil }

func (b *plainBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.got)
}

// settle gives the fire-and-forget goroutine time to publish. Polling up to the
// deadline rather than sleeping a fixed span keeps the passing case fast, and an
// event that arrives late is still a leak.
func (b *plainBus) settle(want int) int {
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if b.count() >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return b.count()
}

// failAfterDB aborts the request after the whole DB step — Emit included — has
// run, but while WithTransaction's next() is still unwound, so the transaction
// rolls back.
//
// Registering this on the *Service* step at After position is load-bearing.
// Emit already returns early when it sees ctx.Response.StatusCode >= 400, so a
// failure inside its own next() is handled today; the EV-3 window is a rollback
// Emit cannot see, which means anything between Emit's return and the commit:
// a Service-After middleware, or the commit itself failing.
func failAfterDB() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		ctx.Abort(http.StatusInternalServerError, "BOOM", "post-commit-window failure")
		return nil
	}
}

func emitTxServer(t *testing.T, bus events.Publisher, withFailure bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{TxNote{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForOperation(maniflex.OpCreate))
			s.Pipeline.DB.Register(
				events.Emit(bus),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After))
			if withFailure {
				// Service/After: inside WithTransaction's next(), outside the DB
				// step's. So it runs after Emit has published and before the
				// commit — the window Emit's own status check cannot cover.
				s.Pipeline.Service.Register(
					failAfterDB(),
					maniflex.ForOperation(maniflex.OpCreate),
					maniflex.AtPosition(maniflex.After))
			}
		},
	})
}

// TestEmitTx_RolledBackWriteEmitsNoEvent is the EV-3 regression. The write is
// rolled back, so no subscriber may ever have been told it happened.
func TestEmitTx_RolledBackWriteEmitsNoEvent(t *testing.T) {
	bus := &plainBus{}
	srv := emitTxServer(t, bus, true)

	resp := srv.POST("/tx_notes", map[string]any{"title": "phantom"})
	if resp.Status < 400 {
		t.Fatalf("precondition: the request must fail so the tx rolls back, got %d", resp.Status)
	}

	if n := bus.settle(1); n != 0 {
		t.Errorf("published %d event(s) for a write that was rolled back: "+
			"subscribers now believe a record exists that does not", n)
	}

	// And the row really is gone — proving the rollback happened, so the
	// assertion above is about ordering and not about a write that succeeded.
	if list := srv.GET("/tx_notes").DataList(); len(list) != 0 {
		t.Fatalf("precondition: %d row(s) survived the rollback; the fixture is wrong", len(list))
	}
}

// ── ctx.AfterCommit semantics ────────────────────────────────────────────────

// recordHook registers an AfterCommit callback from the DB-After position and
// reports whether it was deferred and whether it eventually ran.
func recordHook(deferred *bool, ran *bool) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		*deferred = ctx.AfterCommit(func() { *ran = true })
		return nil
	}
}

func TestAfterCommit_RunsOnCommitAndNotOnRollback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withFailure bool
		wantRan     bool
	}{
		{"commit runs the hook", false, true},
		{"rollback drops the hook", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deferred, ran bool
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{TxNote{}},
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.Service.Register(
						maniflex.WithTransaction(nil),
						maniflex.ForOperation(maniflex.OpCreate))
					s.Pipeline.DB.Register(
						recordHook(&deferred, &ran),
						maniflex.ForOperation(maniflex.OpCreate),
						maniflex.AtPosition(maniflex.After))
					if tc.withFailure {
						s.Pipeline.Service.Register(
							failAfterDB(),
							maniflex.ForOperation(maniflex.OpCreate),
							maniflex.AtPosition(maniflex.After))
					}
				},
			})
			srv.POST("/tx_notes", map[string]any{"title": "x"})

			if !deferred {
				t.Fatal("AfterCommit reported the hook ran inline; under WithTransaction it must defer, " +
					"or this test proves nothing about commit ordering")
			}
			if ran != tc.wantRan {
				t.Errorf("hook ran = %v, want %v", ran, tc.wantRan)
			}
		})
	}
}

// With no transaction there is nothing to wait for, and nothing to drain the
// queue — so the callback must run inline rather than be silently swallowed.
// This is the branch that keeps a deferred side effect from being lost whenever
// WithTransaction is not registered, which is the default.
func TestAfterCommit_WithoutTransactionRunsInline(t *testing.T) {
	var deferred, ran bool
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{TxNote{}},
		Middleware: func(s *maniflex.Server) {
			// Deliberately no WithTransaction.
			s.Pipeline.DB.Register(
				recordHook(&deferred, &ran),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After))
		},
	})
	srv.MustID(srv.POST("/tx_notes", map[string]any{"title": "x"}))

	if deferred {
		t.Error("AfterCommit deferred the hook with no transaction active; nothing would ever run it")
	}
	if !ran {
		t.Error("hook never ran: a side effect registered outside a transaction was lost")
	}
}

// Anti-vacuity: a fix that simply stopped emitting under a transaction would
// pass the test above. A committed write must still produce its event.
func TestEmitTx_CommittedWriteStillEmits(t *testing.T) {
	bus := &plainBus{}
	srv := emitTxServer(t, bus, false)

	srv.MustID(srv.POST("/tx_notes", map[string]any{"title": "real"}))

	if n := bus.settle(1); n != 1 {
		t.Errorf("published %d event(s) for a committed write, want 1", n)
	}
}
