package rabbitmq

// Blocker B2, item 1. New is handed a connection it does not own and cannot
// redial, so a connection or channel drop ended every subscription on it for
// the life of the process: the app kept serving, and the queue silently stopped
// being consumed. NewWithDialer gives the bus a connection of its own, which is
// what makes rebuilding one possible.
//
//	go test ./events/rabbitmq/ -run TestReconnect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xaleel/maniflex/events"
)

// errBrokerDown stands in for a redial that cannot reach the broker.
var errBrokerDown = errors.New("broker is down")

// fakeBroker hands out a fresh channel per open, so a reconnect is observable
// as a second channel rather than inferred from a log line.
type fakeBroker struct {
	mu       sync.Mutex
	channels []*fakeChannel
	openErrs []error
}

func (b *fakeBroker) open() (consumerChannel, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.openErrs) > 0 {
		err := b.openErrs[0]
		b.openErrs = b.openErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	ch := newFakeChannel()
	b.channels = append(b.channels, ch)
	return ch, nil
}

func (b *fakeBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.channels)
}

func (b *fakeBroker) at(i int) *fakeChannel {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.channels[i]
}

// busOnBroker returns a Bus that consumes from br. A non-nil dial marks the bus
// as owning its connection, which is what licenses it to rebuild a
// subscription; the stub is never called, because newConsumer supplies the
// channels directly.
func busOnBroker(br *fakeBroker, reconnects bool, opts ...Options) *Bus {
	b := &Bus{newConsumer: br.open}
	if len(opts) > 0 {
		b.opts = opts[0]
	}
	if b.opts.ReconnectBackoff.Min == 0 {
		b.opts.ReconnectBackoff.Min = time.Millisecond
	}
	if b.opts.ReconnectBackoff.Max == 0 {
		b.opts.ReconnectBackoff.Max = 2 * time.Millisecond
	}
	if reconnects {
		b.dial = func() (*amqp.Connection, error) { return nil, nil }
	}
	return b
}

func TestReconnect_ResumesConsumingAfterTheChannelDrops(t *testing.T) {
	br := &fakeBroker{}
	handled := make(chan uint64, 4)

	cancel, err := busOnBroker(br, true).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"widget.*"},
		Handler: func(_ context.Context, e events.Event) error {
			handled <- 1
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	first, ack := br.at(0), &fakeAcknowledger{}
	first.deliver(t, ack, 1, events.Event{ID: "e1", Type: "widget.created"})
	<-handled

	// The broker drops the channel out from under the consumer.
	close(first.deliveries)

	waitFor(t, "a replacement channel to be opened", func() bool { return br.count() == 2 })

	second := br.at(1)
	second.deliver(t, ack, 2, events.Event{ID: "e2", Type: "widget.created"})
	select {
	case <-handled:
	case <-time.After(3 * time.Second):
		t.Fatal("the resumed subscription did not deliver")
	}
}

// The rebuilt subscription must re-declare everything: a fresh channel knows
// nothing about the exchange, the queue, its bindings, or the prefetch bound.
func TestReconnect_RestoresTopologyAndPrefetch(t *testing.T) {
	br := &fakeBroker{}

	cancel, err := busOnBroker(br, true).Subscribe(context.Background(), events.Subscription{
		Group:       "billing",
		Patterns:    []string{"invoice.*", "receipt.issued"},
		Concurrency: 3,
		Handler:     func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	close(br.at(0).deliveries)
	waitFor(t, "a replacement channel to be opened", func() bool { return br.count() == 2 })

	second := br.at(1)
	waitFor(t, "the queue to be redeclared", func() bool {
		second.mu.Lock()
		defer second.mu.Unlock()
		return len(second.queues) == 1 && len(second.binds) == 2
	})
	if got := second.qosPrefetch(); got != 3 {
		t.Errorf("prefetch on the rebuilt channel = %d, want 3", got)
	}
	second.mu.Lock()
	queue, binds := second.queues[0], append([]string(nil), second.binds...)
	second.mu.Unlock()
	if queue != "maniflex.billing" {
		t.Errorf("queue = %q, want maniflex.billing", queue)
	}
	if binds[0] != "invoice.*" || binds[1] != "receipt.issued" {
		t.Errorf("bindings = %v, want [invoice.* receipt.issued]", binds)
	}
}

// A bus given a connection it does not own keeps the old contract: report the
// death loudly and stop, because it has no way to dial a replacement.
func TestReconnect_WithoutADialerReportsAndStops(t *testing.T) {
	br := &fakeBroker{}
	reported := make(chan string, 1)

	cancel, err := busOnBroker(br, false, Options{
		OnSubscriptionClosed: func(queue string, _ error) { reported <- queue },
	}).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	close(br.at(0).deliveries)

	select {
	case queue := <-reported:
		if queue != "maniflex.default" {
			t.Errorf("reported queue %q, want maniflex.default", queue)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a dropped subscription was not reported")
	}
	if got := br.count(); got != 1 {
		t.Errorf("opened %d channels, want 1: a bus with no dialer must not rebuild", got)
	}
}

// Cancel while the consumer is between reconnect attempts must return, not
// wait out the backoff or spin for ever.
func TestReconnect_CancelDuringBackoffReturns(t *testing.T) {
	br := &fakeBroker{openErrs: []error{nil, errBrokerDown, errBrokerDown, errBrokerDown}}

	cancel, err := busOnBroker(br, true, Options{
		ReconnectBackoff: events.ReadBackoff{Min: time.Second, Max: time.Second},
	}).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	close(br.at(0).deliveries)
	waitFor(t, "the first reconnect attempt to fail", func() bool { return br.count() == 1 })

	returned := make(chan struct{})
	go func() { cancel(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel did not return while the consumer was in reconnect backoff")
	}
}

// An orderly Cancel is not a drop: it must not trigger a reconnect.
func TestReconnect_CancelDoesNotRebuild(t *testing.T) {
	br := &fakeBroker{}

	cancel, err := busOnBroker(br, true).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler:  func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()
	time.Sleep(20 * time.Millisecond)

	if got := br.count(); got != 1 {
		t.Errorf("opened %d channels after Cancel, want 1", got)
	}
}

// The loop cannot test its own cancellation arm: Cancel closes the delivery
// channel, so a shutdown and a drop can both be ready at the same instant and
// which one the select picks is not the test's to arrange. The decision is a
// function for exactly that reason, and this covers every combination of it.
func TestAfterSession(t *testing.T) {
	cases := []struct {
		name      string
		dropped   bool
		ctxErr    error
		canRedial bool
		want      sessionOutcome
		why       string
	}{
		{
			name: "cancelled, no drop", dropped: false, ctxErr: context.Canceled,
			canRedial: true, want: stopQuietly,
			why: "the caller asked for this",
		},
		{
			name: "dropped while shutting down", dropped: true, ctxErr: context.Canceled,
			canRedial: true, want: stopQuietly,
			why: "Cancel closes the delivery channel too, so a shutdown surfaces " +
				"as a drop — rebuilding here opens a channel after the caller " +
				"believed the subscription was gone",
		},
		{
			name: "dropped, bus owns its connection", dropped: true, ctxErr: nil,
			canRedial: true, want: rebuild,
			why: "the outage this blocker exists to end",
		},
		{
			name: "dropped, connection belongs to the caller", dropped: true, ctxErr: nil,
			canRedial: false, want: reportAndStop,
			why: "a bus built by New cannot dial a replacement, so the death is " +
				"made loud instead",
		},
		{
			name: "no drop, no cancellation", dropped: false, ctxErr: nil,
			canRedial: true, want: stopQuietly,
			why: "the session ended for neither reason; do nothing rather than loop",
		},
		{
			name: "cancelled and cannot redial", dropped: true, ctxErr: context.Canceled,
			canRedial: false, want: stopQuietly,
			why: "an orderly shutdown is not an outage, whoever owns the connection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := afterSession(tc.dropped, tc.ctxErr, tc.canRedial)
			if got != tc.want {
				t.Errorf("afterSession(dropped=%v, ctxErr=%v, canRedial=%v) = %d, want %d — %s",
					tc.dropped, tc.ctxErr, tc.canRedial, got, tc.want, tc.why)
			}
		})
	}
}
