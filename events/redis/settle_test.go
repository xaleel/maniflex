package redis

// Audit C5: the consumer acknowledged a message as soon as DeliverWithRetry
// returned, whatever had become of the event — because DeliverWithRetry returned
// nothing. It now reports whether the event was settled, and the ack is
// conditional on that.
//
// Redis is the adapter these can be written against: its Ack goes through the
// streamOps seam, where the NATS, Kafka and RabbitMQ adapters call Ack on the
// broker client's own message type. The contract itself is covered in events/.
//
//	go test ./events/redis/ -run TestDispatch_

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/events"
)

// runDispatch pushes one message through the dispatch path and waits for it.
func runDispatch(t *testing.T, ctx context.Context, b *Bus, sub events.Subscription, m goredis.XMessage) {
	t.Helper()
	sem := make(chan struct{}, 1)
	var wg sync.WaitGroup
	b.dispatch(ctx, "test:stream", sub, []goredis.XMessage{m}, sem, &wg)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
}

// The shutdown case. Delivery was abandoned between two attempts, so nothing was
// decided about this event — acking it destroys it.
func TestDispatch_LeavesAnAbandonedDeliveryUnacked(t *testing.T) {
	ops := &fakeOps{}
	ctx, cancel := context.WithCancel(context.Background())
	sub := events.Subscription{
		Group: "g", Patterns: []string{"*"}, MaxRetry: 3,
		// An hour, so the retry loop can only leave via the cancellation arm.
		Backoff: func(int) time.Duration { return time.Hour },
		Handler: func(context.Context, events.Event) error {
			cancel()
			return errors.New("handler is broken")
		},
	}

	runDispatch(t, ctx, busWith(ops), sub, msg("1-0", "widget.created"))

	if acked := ops.ackedIDs(); len(acked) != 0 {
		t.Errorf("acked %v after abandoning the delivery mid-retry: the message leaves the "+
			"pending list without having been handled, so a shutdown between two attempts "+
			"destroys the event", acked)
	}
}

// Anti-vacuity: a delivered message must still be acked, or the fix above is
// indistinguishable from never acking at all and every event is redelivered.
func TestDispatch_AcksADeliveredMessage(t *testing.T) {
	ops := &fakeOps{}
	sub := events.Subscription{
		Group: "g", Patterns: []string{"*"},
		Handler: func(context.Context, events.Event) error { return nil },
	}

	runDispatch(t, context.Background(), busWith(ops), sub, msg("2-0", "widget.created"))

	if acked := ops.ackedIDs(); len(acked) != 1 || acked[0] != "2-0" {
		t.Errorf("acked %v after a successful delivery, want [2-0]: the message stays pending "+
			"and is reclaimed and redelivered for ever", acked)
	}
}

// The deliberate drop stays acked. Withholding the ack from a message whose
// handler fails deterministically means reclaiming and redelivering it for ever,
// which is worse than the logged loss it replaces.
func TestDispatch_AcksAnExhaustedDeliveryWithNoDLQ(t *testing.T) {
	ops := &fakeOps{}
	sub := events.Subscription{
		Group: "g", Patterns: []string{"*"}, MaxRetry: 0,
		Handler: func(context.Context, events.Event) error { return errors.New("handler is broken") },
	}

	runDispatch(t, context.Background(), busWith(ops), sub, msg("3-0", "widget.created"))

	if acked := ops.ackedIDs(); len(acked) != 1 || acked[0] != "3-0" {
		t.Errorf("acked %v after exhausting retries with no DLQ, want [3-0]: the message is "+
			"reclaimed and redelivered on every sweep, for ever", acked)
	}
}
