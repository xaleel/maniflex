package rabbitmq

// Blocker B2, item 3: this adapter had six tests, all on helpers. Publish,
// Subscribe, and the whole ack path were untested, because reaching them needed
// a broker. events/redis solved the same problem with a seam over the commands
// its consumer issues; this is that seam for AMQP.
//
// amqp.Delivery carries an exported Acknowledger, so a fake can observe what the
// consumer does with each message without a broker anywhere.
//
//	go test ./events/rabbitmq/ -run TestSubscribe

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xaleel/maniflex/events"
)

type ackRecord struct {
	tag     uint64
	requeue bool
}

// fakeAcknowledger records what the consumer decided about each delivery.
type fakeAcknowledger struct {
	mu    sync.Mutex
	acked []uint64
	nacks []ackRecord
}

func (f *fakeAcknowledger) Ack(tag uint64, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, tag)
	return nil
}

func (f *fakeAcknowledger) Nack(tag uint64, _, requeue bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nacks = append(f.nacks, ackRecord{tag: tag, requeue: requeue})
	return nil
}

func (f *fakeAcknowledger) Reject(tag uint64, requeue bool) error {
	return f.Nack(tag, false, requeue)
}

func (f *fakeAcknowledger) ackedTags() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.acked...)
}

func (f *fakeAcknowledger) nackedTags() []ackRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ackRecord(nil), f.nacks...)
}

// fakeChannel stands in for the AMQP channel a subscription consumes on.
type fakeChannel struct {
	mu         sync.Mutex
	deliveries chan amqp.Delivery
	closes     chan *amqp.Error
	prefetch   int
	queues     []string
	binds      []string
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{
		deliveries: make(chan amqp.Delivery, 16),
		closes:     make(chan *amqp.Error, 1),
		prefetch:   -1,
	}
}

func (f *fakeChannel) ExchangeDeclare() error { return nil }

func (f *fakeChannel) QueueDeclare(queue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues = append(f.queues, queue)
	return nil
}

func (f *fakeChannel) QueueBind(_, bindingKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binds = append(f.binds, bindingKey)
	return nil
}

func (f *fakeChannel) Qos(prefetch int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefetch = prefetch
	return nil
}

func (f *fakeChannel) Consume(string) (<-chan amqp.Delivery, error) { return f.deliveries, nil }
func (f *fakeChannel) NotifyClose() <-chan *amqp.Error              { return f.closes }
func (f *fakeChannel) Close() error                                 { return nil }

func (f *fakeChannel) qosPrefetch() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefetch
}

// deliver pushes one event onto the delivery channel under the given tag.
func (f *fakeChannel) deliver(t *testing.T, ack amqp.Acknowledger, tag uint64, e events.Event) {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	f.deliveries <- amqp.Delivery{Acknowledger: ack, DeliveryTag: tag, Body: body}
}

func (f *fakeChannel) deliverRaw(ack amqp.Acknowledger, tag uint64, body []byte) {
	f.deliveries <- amqp.Delivery{Acknowledger: ack, DeliveryTag: tag, Body: body}
}

// busOn returns a Bus whose subscriptions consume from ch.
func busOn(ch *fakeChannel, opts ...Options) *Bus {
	b := &Bus{newConsumer: func() (consumerChannel, error) { return ch, nil }}
	if len(opts) > 0 {
		b.opts = opts[0]
	}
	return b
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSubscribe_AcksAMessageItsHandlerAccepted(t *testing.T) {
	ch, ack := newFakeChannel(), &fakeAcknowledger{}
	handled := make(chan struct{}, 1)

	cancel, err := busOn(ch).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"widget.*"},
		Handler: func(context.Context, events.Event) error {
			handled <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ch.deliver(t, ack, 7, events.Event{ID: "e1", Type: "widget.created"})

	<-handled
	waitFor(t, "the delivery to be acked", func() bool { return len(ack.ackedTags()) == 1 })
	if got := ack.ackedTags(); got[0] != 7 {
		t.Errorf("acked tag %d, want 7", got[0])
	}
}

// A body that will not parse is not a delivery failure to retry. No attempt can
// ever handle it, so it is rejected without requeue rather than redelivered for
// ever.
func TestSubscribe_NacksAMessageThatWillNotParse(t *testing.T) {
	ch, ack := newFakeChannel(), &fakeAcknowledger{}

	cancel, err := busOn(ch).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler: func(context.Context, events.Event) error {
			t.Error("the handler saw a message that does not parse")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ch.deliverRaw(ack, 3, []byte("{not json"))

	waitFor(t, "the delivery to be nacked", func() bool { return len(ack.nackedTags()) == 1 })
	got := ack.nackedTags()[0]
	if got.tag != 3 {
		t.Errorf("nacked tag %d, want 3", got.tag)
	}
	if got.requeue {
		t.Error("nacked with requeue: a body that cannot parse would be redelivered for ever")
	}
}

// The queue is bound per pattern, but a shared group queue can still carry
// types this subscription did not ask for. Those are acked, not left pending.
func TestSubscribe_AcksANonMatchingMessageWithoutCallingTheHandler(t *testing.T) {
	ch, ack := newFakeChannel(), &fakeAcknowledger{}

	cancel, err := busOn(ch).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"widget.*"},
		Handler: func(context.Context, events.Event) error {
			t.Error("the handler saw an event outside its patterns")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ch.deliver(t, ack, 11, events.Event{ID: "e2", Type: "invoice.paid"})

	waitFor(t, "the non-matching delivery to be acked", func() bool { return len(ack.ackedTags()) == 1 })
}

// B2 item 2. Without a Qos the broker pushes the whole queue at the consumer,
// so "unacked messages cannot accumulate without bound" was not true of this
// adapter, which is the reason its settle rule departs from redis and nats.
func TestSubscribe_BoundsPrefetch(t *testing.T) {
	ch := newFakeChannel()

	cancel, err := busOn(ch).Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: 4,
		Handler:     func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if got := ch.qosPrefetch(); got != 4 {
		t.Errorf("prefetch = %d, want 4 (one in-flight message per worker)", got)
	}
}

func TestSubscribe_PrefetchIsConfigurable(t *testing.T) {
	ch := newFakeChannel()

	cancel, err := busOn(ch, Options{Prefetch: 32}).Subscribe(context.Background(), events.Subscription{
		Patterns:    []string{"*"},
		Concurrency: 4,
		Handler:     func(context.Context, events.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if got := ch.qosPrefetch(); got != 32 {
		t.Errorf("prefetch = %d, want the configured 32", got)
	}
}

func TestSubscribe_CancelWaitsForAnInFlightHandler(t *testing.T) {
	ch, ack := newFakeChannel(), &fakeAcknowledger{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished sync.WaitGroup
	finished.Add(1)

	cancel, err := busOn(ch).Subscribe(context.Background(), events.Subscription{
		Patterns: []string{"*"},
		Handler: func(context.Context, events.Event) error {
			close(entered)
			<-release
			finished.Done()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ch.deliver(t, ack, 1, events.Event{ID: "e3", Type: "widget.created"})
	<-entered

	returned := make(chan struct{})
	go func() { cancel(); close(returned) }()

	select {
	case <-returned:
		t.Fatal("Cancel returned while a handler was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	finished.Wait()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel did not return after the handler finished")
	}
}
