// Package rabbitmq provides an AMQP 0.9.1 (RabbitMQ) event bus adapter for maniflex events.
//
// # Architecture
//
// RabbitMQ is queue-based, not log-based. Once a message is acknowledged, it is gone;
// there is no offset-rewind or replay. This is a fundamental difference from the
// streaming brokers (Redis, NATS, Kafka).
//
// Mapping:
//   - Publish uses a topic exchange. The routing key is set to event.Type
//     (e.g. "invoice.created"), matching the pattern directly.
//   - Subscribe.Group becomes a named durable queue with competing consumers.
//   - Pattern matching ("invoice.*") becomes a topic-exchange binding with the
//     routing key pattern as the binding key.
//
// # Replay warning
//
// RabbitMQ does NOT provide replay. Teams that need replay should pair this adapter
// with events/outbox (which keeps the durable log in the application DB) or choose
// a streaming broker (Redis Streams, NATS JetStream, Kafka).
//
// # No reconnection
//
// This adapter does not reconnect. New takes a *amqp.Connection it does not own,
// and amqp091-go connections do not self-heal, so a connection or channel drop
// ends every subscription running on it permanently — the process keeps serving
// and the queue silently stops being consumed.
//
// A subscription that dies logs an ERROR naming the queue, and Options
// OnSubscriptionClosed is called so the app can alert or rebuild the bus on a
// fresh connection. Supervise that callback if consumer downtime matters:
// nothing below it will bring the subscription back.
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xaleel/maniflex/events"
)

const exchangeName = "maniflex.events"

// defaultConfirmTimeout bounds the wait for a broker confirm, so a wedged or
// unreachable broker fails the publish instead of blocking the caller forever.
const defaultConfirmTimeout = 5 * time.Second

// Options customises the Bus. All fields are optional.
type Options struct {
	// OnSubscriptionClosed is called when a subscription stops for a reason
	// other than its Cancel being invoked — a connection or channel drop, or
	// the queue being deleted from under it.
	//
	// This adapter does not reconnect: amqp091-go connections do not self-heal,
	// and New is handed a connection it does not own and cannot redial. Without
	// this callback a drop leaves the subscription dead while the process keeps
	// running and looking healthy, which is the failure this exists to surface.
	// Use it to alert, or to tear down and re-Subscribe on a fresh connection.
	//
	// It is called once per subscription, from the subscription's own goroutine.
	OnSubscriptionClosed func(queue string, err error)

	// ConfirmTimeout bounds how long Publish waits for the broker to confirm a
	// message. Default: 5s.
	ConfirmTimeout time.Duration
}

// Bus is an AMQP 0.9.1 event bus.
type Bus struct {
	conn  *amqp.Connection
	opts  Options
	mu    sync.Mutex
	pubCh *amqp.Channel // shared publish channel (lazy-opened, re-opened on error)
}

// New creates a RabbitMQ Bus from an existing AMQP connection.
// The topic exchange "maniflex.events" is declared as durable on first use.
//
// The publish channel runs in confirm mode, so Publish reports whether the
// broker actually took responsibility for the message rather than only whether
// it was written to a socket.
func New(conn *amqp.Connection, opts ...Options) (*Bus, error) {
	b := &Bus{conn: conn}
	if len(opts) > 0 {
		b.opts = opts[0]
	}
	if b.opts.ConfirmTimeout <= 0 {
		b.opts.ConfirmTimeout = defaultConfirmTimeout
	}
	ch, err := b.openPublishChannel()
	if err != nil {
		return nil, err
	}
	if err := declareExchange(ch); err != nil {
		ch.Close()
		return nil, err
	}
	b.pubCh = ch
	return b, nil
}

// Publish routes e to the topic exchange with routing key = e.Type.
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("rabbitmq: marshal: %w", err)
	}

	b.mu.Lock()
	ch := b.pubCh
	b.mu.Unlock()

	conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchangeName, e.Type, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    e.ID,
			Timestamp:    e.Time,
			Body:         payload,
		})
	if err != nil {
		b.reopenPublishChannel()
		return fmt.Errorf("rabbitmq: publish: %w", err)
	}

	// Writing to the socket is not delivery. Without waiting for the confirm,
	// Publish returned nil for a message the broker never persisted — an
	// unroutable event, a full disk, a node failing mid-write all looked like
	// success, and the event was gone with nothing to retry from (audit EV-5).
	wctx, cancel := context.WithTimeout(ctx, b.opts.ConfirmTimeout)
	defer cancel()
	acked, err := conf.WaitContext(wctx)
	switch {
	case err != nil:
		// Timed out or the caller's context ended: the broker's answer is
		// unknown, so the message may or may not be stored. Reported as a
		// failure because the safe reading of "unknown" is "not delivered" —
		// a redelivery is a duplicate, which dedupe handles, while assuming
		// success loses the event outright.
		b.reopenPublishChannel()
		return fmt.Errorf("rabbitmq: publish confirm for %s: %w", e.ID, err)
	case !acked:
		// The broker explicitly refused responsibility for the message.
		return fmt.Errorf("rabbitmq: broker nacked event %s (type %s)", e.ID, e.Type)
	}
	return nil
}

// reopenPublishChannel replaces the shared publish channel after an error on it.
// A channel is closed by the broker on any protocol error, so the next publish
// on it would fail too.
func (b *Bus) reopenPublishChannel() {
	b.mu.Lock()
	defer b.mu.Unlock()
	newCh, err := b.openPublishChannel()
	if err != nil {
		// Leave the old channel in place: the next publish fails against it and
		// tries again from here. Replacing it with nil would panic instead.
		slog.Default().Error("rabbitmq: could not reopen publish channel",
			slog.String("error", err.Error()))
		return
	}
	if b.pubCh != nil {
		b.pubCh.Close()
	}
	b.pubCh = newCh
}

// PublishBatch publishes all events in es sequentially on the shared channel.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe declares a durable queue bound to the exchange with each pattern
// as the binding key, then starts competing consumers for sub.Group.
//
// Pattern "invoice.*" binds to routing keys matching that AMQP topic pattern.
// Pattern "*" binds to "#" (all routing keys).
func (b *Bus) Subscribe(ctx context.Context, sub events.Subscription) (events.Cancel, error) {
	if sub.Concurrency <= 0 {
		sub.Concurrency = 1
	}
	if sub.MaxRetry <= 0 {
		sub.MaxRetry = 3
	}
	if sub.Backoff == nil {
		sub.Backoff = func(n int) time.Duration { return time.Duration(n) * time.Second }
	}
	if sub.Group == "" {
		sub.Group = "default"
	}
	if len(sub.Patterns) == 0 {
		sub.Patterns = []string{"*"}
	}

	ch, err := b.openChannel()
	if err != nil {
		return nil, err
	}
	if err := declareExchange(ch); err != nil {
		ch.Close()
		return nil, err
	}

	// Declare a durable queue for this consumer group.
	queue := fmt.Sprintf("maniflex.%s", sub.Group)
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		ch.Close()
		return nil, fmt.Errorf("rabbitmq: declare queue: %w", err)
	}

	// Bind the queue to each pattern.
	for _, pattern := range sub.Patterns {
		bindKey := patternToBindingKey(pattern)
		if err := ch.QueueBind(queue, bindKey, exchangeName, false, nil); err != nil {
			ch.Close()
			return nil, fmt.Errorf("rabbitmq: bind %q: %w", bindKey, err)
		}
	}

	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		ch.Close()
		return nil, fmt.Errorf("rabbitmq: consume: %w", err)
	}

	// The broker's reason for closing this channel. Read only after deliveries
	// closes, by which point amqp091-go has already delivered the error (or
	// closed the channel, giving nil for a clean shutdown).
	closeErr := ch.NotifyClose(make(chan *amqp.Error, 1))

	cctx, cancel := context.WithCancel(ctx)
	sem := make(chan struct{}, sub.Concurrency)
	var wg sync.WaitGroup

	go func() {
		defer ch.Close()
		for {
			select {
			case <-cctx.Done():
				wg.Wait()
				return
			case msg, ok := <-deliveries:
				if !ok {
					// The delivery channel closed on us. Any connection or
					// channel drop lands here, and this adapter does not
					// reconnect — so the subscription is now dead for the life
					// of the process while everything else keeps running.
					// Returning quietly, as this used to, made a total consumer
					// outage indistinguishable from a healthy idle service
					// (audit EV-5).
					wg.Wait()
					b.reportSubscriptionClosed(cctx, queue, closeErr)
					return
				}
				var e events.Event
				if err := json.Unmarshal(msg.Body, &e); err != nil {
					msg.Nack(false, false)
					continue
				}
				if !matchesAny(sub.Patterns, e.Type) {
					msg.Ack(false)
					continue
				}
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() {
						<-sem
						wg.Done()
					}()
					events.DeliverWithRetry(cctx, b, sub, e)
					msg.Ack(false)
				}()
			}
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}, nil
}

// reportSubscriptionClosed announces that a subscription has stopped consuming.
//
// A cancelled context means the caller asked for this, so it is not reported:
// Cancel closes the delivery channel too, and treating an orderly shutdown as an
// outage would train operators to ignore the alert that matters.
func (b *Bus) reportSubscriptionClosed(ctx context.Context, queue string, closeErr <-chan *amqp.Error) {
	if ctx.Err() != nil {
		return
	}

	var err error
	select {
	case amqpErr := <-closeErr:
		if amqpErr != nil {
			err = amqpErr
		}
	default:
	}
	if err == nil {
		// The delivery channel closed without the broker giving a reason —
		// a basic.cancel (the queue was deleted) reaches us this way.
		err = fmt.Errorf("delivery channel closed without an error; the queue may have been deleted")
	}

	slog.Default().Error("rabbitmq: subscription stopped and will not reconnect",
		slog.String("queue", queue),
		slog.String("error", err.Error()),
		slog.String("impact", "events routed to this queue are no longer consumed"))

	if b.opts.OnSubscriptionClosed != nil {
		b.opts.OnSubscriptionClosed(queue, err)
	}
}

// Close closes the underlying AMQP connection.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pubCh != nil {
		b.pubCh.Close()
	}
	return b.conn.Close()
}

func (b *Bus) openChannel() (*amqp.Channel, error) {
	ch, err := b.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	return ch, nil
}

// openPublishChannel opens a channel in confirm mode. Confirm mode is per
// channel and cannot be turned on later, so every replacement publish channel
// must go through here — a plain openChannel would silently downgrade Publish
// to fire-and-forget after the first reconnect.
func (b *Bus) openPublishChannel() (*amqp.Channel, error) {
	ch, err := b.openChannel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		ch.Close()
		return nil, fmt.Errorf("rabbitmq: enable publisher confirms: %w", err)
	}
	return ch, nil
}

func declareExchange(ch *amqp.Channel) error {
	return ch.ExchangeDeclare(exchangeName, "topic", true, false, false, false, nil)
}

// patternToBindingKey converts a glob pattern to an AMQP topic binding key.
// "*" → "#" (all), "invoice.*" → "invoice.*", "invoice.created" → "invoice.created".
func patternToBindingKey(pattern string) string {
	if pattern == "*" {
		return "#"
	}
	return pattern
}

func matchesAny(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, _ := path.Match(p, eventType); ok {
			return true
		}
	}
	return false
}
