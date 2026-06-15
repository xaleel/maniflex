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
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"maniflex/events"
)

const exchangeName = "maniflex.events"

// Bus is an AMQP 0.9.1 event bus.
type Bus struct {
	conn    *amqp.Connection
	mu      sync.Mutex
	pubCh   *amqp.Channel // shared publish channel (lazy-opened, re-opened on error)
}

// New creates a RabbitMQ Bus from an existing AMQP connection.
// The topic exchange "maniflex.events" is declared as durable on first use.
func New(conn *amqp.Connection) (*Bus, error) {
	b := &Bus{conn: conn}
	ch, err := b.openChannel()
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

	err = ch.PublishWithContext(ctx, exchangeName, e.Type, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    e.ID,
		Timestamp:    e.Time,
		Body:         payload,
	})
	if err != nil {
		// Re-open channel on error (connection drop etc.)
		b.mu.Lock()
		newCh, rerr := b.openChannel()
		if rerr == nil {
			b.pubCh = newCh
		}
		b.mu.Unlock()
		return fmt.Errorf("rabbitmq: publish: %w", err)
	}
	return nil
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

