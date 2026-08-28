package events

// Audit C5: a broker adapter acknowledged an event as soon as DeliverWithRetry
// returned, whatever had happened to it — because DeliverWithRetry returned
// nothing, so there was nothing else it could do. Two of those outcomes are not
// deliveries at all:
//
//   - ctx cancelled during a retry backoff. DeliverWithRetry gave up and
//     returned without logging anything, and the adapter acked. A shutdown that
//     landed while a handler was between attempts therefore destroyed the event
//     in silence — at exactly the moment a deploy makes that most likely.
//   - dead-lettering configured and its publish failed. The safety net the
//     operator asked for did not catch the event, and the adapter acked anyway.
//
// The third, retries exhausted with no DLQ, is a real drop and stays one:
// withholding the ack there would have the broker redeliver a message that
// fails deterministically, forever. That is the audit's suggested fix and it is
// the wrong one — it turns a bounded loss into an unbounded loop.
//
//	go test ./events/ -run TestDeliverWithRetry_Settle

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// failingPublisher stands in for a broker that will not take the DLQ event.
type failingPublisher struct{ err error }

func (p failingPublisher) Publish(context.Context, Event) error        { return p.err }
func (p failingPublisher) PublishBatch(context.Context, []Event) error { return p.err }
func (p failingPublisher) Close() error                                { return nil }

func alwaysFails(context.Context, Event) error { return errors.New("handler is broken") }

// A delivered event is settled: the adapter should acknowledge it.
func TestDeliverWithRetry_SettleOnSuccess(t *testing.T) {
	sub := Subscription{MaxRetry: 3, Handler: func(context.Context, Event) error { return nil }}
	if settled := DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "s1"}); !settled {
		t.Error("a handler that succeeded reported the event unsettled, so the adapter " +
			"will leave it for redelivery and the subscriber sees it twice")
	}
}

// Dead-lettered is settled too: the event has been disposed of, just not by the
// handler.
func TestDeliverWithRetry_SettleOnDeadLetter(t *testing.T) {
	pub := &recordingPublisher{}
	sub := Subscription{MaxRetry: 0, DLQ: "widget.dlq", Handler: alwaysFails}

	if settled := DeliverWithRetry(context.Background(), pub, sub, Event{ID: "s2"}); !settled {
		t.Error("a dead-lettered event reported unsettled: the adapter would redeliver it " +
			"and dead-letter it again on every pass")
	}
	if len(pub.published) != 1 {
		t.Fatalf("%d DLQ publishes, want 1", len(pub.published))
	}
}

// No DLQ configured is a deliberate, logged drop — and it must stay settled.
// Redelivering an event whose handler fails every time never converges.
func TestDeliverWithRetry_SettleOnDropWithNoDLQ(t *testing.T) {
	logs := captureLogs(t)
	sub := Subscription{MaxRetry: 0, Handler: alwaysFails}

	if settled := DeliverWithRetry(context.Background(), &recordingPublisher{}, sub, Event{ID: "s3"}); !settled {
		t.Error("an exhausted delivery with no DLQ reported unsettled: the broker would " +
			"redeliver a message that fails deterministically, forever")
	}
	if n := countLevel(*logs, slog.LevelError); n != 1 {
		t.Errorf("%d ERROR logs for a dropped event, want exactly 1", n)
	}
}

// The safety net failed. The event is not disposed of, so the adapter must not
// claim it is: leaving it unacked lets the broker bring it back, DLQ publish and
// all.
func TestDeliverWithRetry_UnsettledWhenTheDLQPublishFails(t *testing.T) {
	captureLogs(t)
	sub := Subscription{MaxRetry: 0, DLQ: "widget.dlq", Handler: alwaysFails}
	pub := failingPublisher{err: errors.New("broker unreachable")}

	if settled := DeliverWithRetry(context.Background(), pub, sub, Event{ID: "s4"}); settled {
		t.Error("a failed dead-letter publish reported the event settled: the adapter acks, " +
			"and an event the operator asked to have kept is gone")
	}
}

// The shutdown case. Nothing has been decided about this event — it was not
// delivered, not dead-lettered, not dropped — so it must come back.
func TestDeliverWithRetry_UnsettledWhenCancelledMidRetry(t *testing.T) {
	captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	sub := Subscription{
		MaxRetry: 3,
		// An hour, so the only way this returns promptly is the cancellation
		// arm: no timing assumption is doing the work here.
		Backoff: func(int) time.Duration { return time.Hour },
		Handler: func(context.Context, Event) error {
			cancel()
			return errors.New("handler is broken")
		},
	}

	done := make(chan bool, 1)
	go func() { done <- DeliverWithRetry(ctx, &recordingPublisher{}, sub, Event{ID: "s5"}) }()

	select {
	case settled := <-done:
		if settled {
			t.Error("a delivery abandoned mid-retry reported the event settled: the adapter " +
				"acks it, so a shutdown between two attempts destroys the event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeliverWithRetry did not return on a cancelled context")
	}
}

// And it must say so. This path returned bare, so the one outcome that loses an
// event through no fault of the handler was the only one that left no trace.
func TestDeliverWithRetry_LogsWhenAbandonedMidRetry(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	sub := Subscription{
		MaxRetry: 3,
		Backoff:  func(int) time.Duration { return time.Hour },
		Handler: func(context.Context, Event) error {
			cancel()
			return errors.New("handler is broken")
		},
	}

	done := make(chan struct{})
	go func() { DeliverWithRetry(ctx, &recordingPublisher{}, sub, Event{ID: "s6"}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DeliverWithRetry did not return on a cancelled context")
	}

	var found bool
	for _, l := range *logs {
		if l.level >= slog.LevelWarn && l.attrs["id"] == "s6" && l.msg != "events: handler failed, retrying" {
			found = true
		}
	}
	if !found {
		t.Errorf("abandoning a delivery mid-retry logged nothing above the per-attempt "+
			"retry warning; got %d records: an operator has no way to know the event "+
			"was left unfinished", len(*logs))
	}
}
