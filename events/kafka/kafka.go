// Package kafka provides a kafka-go event bus adapter for maniflex events.
//
// Events are published to Kafka topics derived from the event Type
// (e.g. "invoice.created"). The event Subject is used as the partition key
// for consistent ordering of records related to the same entity.
//
// Consumer groups provide at-least-once delivery. Pattern matching is done
// client-side after reading from matching topics.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"maniflex/events"
)

// Config holds optional Kafka topic configuration applied when EnsureTopics
// creates new topics. Zero values fall back to the defaults below.
type Config struct {
	// NumPartitions is the number of partitions for newly created topics.
	// Default: 3. A value of 1 disables the per-Subject partition key benefit.
	NumPartitions int
	// ReplicationFactor is the replication factor for newly created topics.
	// Default: 1 (suitable for single-broker dev clusters).
	ReplicationFactor int
}

// Bus is a kafka-go event bus.
type Bus struct {
	brokers        []string
	prefix         string
	cfg            Config
	writer         *kafkago.Writer
	ensuredTopics  sync.Map // topic string → struct{}
}

// New creates a Kafka Bus. brokers is a list of bootstrap broker addresses
// ("localhost:9092"). prefix is prepended to topic names with a dot separator
// ("myapp" → topic "myapp.invoice.created").
//
// A single goroutine-safe Writer is held for the lifetime of the Bus; call
// Close when the Bus is no longer needed.
func New(brokers []string, prefix string, cfgs ...Config) *Bus {
	cfg := Config{NumPartitions: 3, ReplicationFactor: 1}
	if len(cfgs) > 0 {
		if cfgs[0].NumPartitions > 0 {
			cfg.NumPartitions = cfgs[0].NumPartitions
		}
		if cfgs[0].ReplicationFactor > 0 {
			cfg.ReplicationFactor = cfgs[0].ReplicationFactor
		}
	}
	w := kafkago.NewWriter(kafkago.WriterConfig{
		Brokers:  brokers,
		Balancer: &kafkago.Hash{},
		// Topic is intentionally empty: each Message.Topic is used instead,
		// allowing the single writer to route to any topic.
	})
	return &Bus{brokers: brokers, prefix: prefix, cfg: cfg, writer: w}
}

func (b *Bus) topic(eventType string) string {
	if b.prefix != "" {
		return b.prefix + "." + eventType
	}
	return eventType
}

// Publish writes e to the topic derived from e.Type.
// The partition key is set to e.Subject for ordered delivery per entity.
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("kafka: marshal: %w", err)
	}
	topic := b.topic(e.Type)
	if err := b.ensureTopicOnce(ctx, topic); err != nil {
		return err
	}
	return b.writer.WriteMessages(ctx, kafkago.Message{
		Topic: topic,
		Key:   []byte(e.Subject),
		Value: payload,
	})
}

// PublishBatch publishes all events using the shared writer in a single call.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	msgs := make([]kafkago.Message, 0, len(es))
	for _, e := range es {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("kafka: marshal: %w", err)
		}
		topic := b.topic(e.Type)
		if err := b.ensureTopicOnce(ctx, topic); err != nil {
			return err
		}
		msgs = append(msgs, kafkago.Message{
			Topic: topic,
			Key:   []byte(e.Subject),
			Value: payload,
		})
	}
	return b.writer.WriteMessages(ctx, msgs...)
}

// Subscribe starts one consumer-group reader per pattern and delivers matching
// events to sub.Handler. Each pattern maps to a topic (exact or prefix-based).
// Wildcard "*" subscribes to all topics by reading from the first available topic.
//
// Note: Kafka does not support server-side topic pattern matching. Patterns are
// resolved to explicit topic names; "*" reads from a hub topic "prefix.*" that
// receives every event. Use explicit patterns for production workloads.
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

	cctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	for _, pattern := range sub.Patterns {
		pattern := pattern
		topic := b.topic(pattern)
		if pattern == "*" {
			topic = b.topic("*") // hub topic
		}

		r := kafkago.NewReader(kafkago.ReaderConfig{
			Brokers: b.brokers,
			GroupID: sub.Group,
			Topic:   topic,
		})

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			b.runConsumer(cctx, r, sub)
		}()
	}

	return func() {
		cancel()
		wg.Wait()
	}, nil
}

func (b *Bus) runConsumer(ctx context.Context, r *kafkago.Reader, sub events.Subscription) {
	sem := make(chan struct{}, sub.Concurrency)
	var wg sync.WaitGroup

	for {
		msg, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return
			}
			time.Sleep(time.Second)
			continue
		}

		var e events.Event
		if err := json.Unmarshal(msg.Value, &e); err != nil {
			_ = r.CommitMessages(ctx, msg)
			continue
		}
		if !matchesAny(sub.Patterns, e.Type) {
			_ = r.CommitMessages(ctx, msg)
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			events.DeliverWithRetry(ctx, b, sub, e)
			_ = r.CommitMessages(ctx, msg)
		}()
	}
}

// Close closes the shared writer. Readers are closed per-subscription via Cancel.
func (b *Bus) Close() error { return b.writer.Close() }

// EnsureTopics creates the given topic names if they do not already exist.
// Call at startup before publishing to guarantee topics exist with the
// configured NumPartitions and ReplicationFactor.
func (b *Bus) EnsureTopics(ctx context.Context, types ...string) error {
	for _, t := range types {
		topic := b.topic(t)
		if err := b.createTopic(ctx, topic); err != nil {
			return err
		}
		b.ensuredTopics.Store(topic, struct{}{})
	}
	return nil
}

// ensureTopicOnce creates topic lazily the first time it is published to.
func (b *Bus) ensureTopicOnce(ctx context.Context, topic string) error {
	if _, ok := b.ensuredTopics.Load(topic); ok {
		return nil
	}
	if err := b.createTopic(ctx, topic); err != nil {
		return err
	}
	b.ensuredTopics.Store(topic, struct{}{})
	return nil
}

// createTopic opens a controller connection and creates the topic.
// Silently ignores "topic already exists" errors.
func (b *Bus) createTopic(ctx context.Context, topic string) error {
	if len(b.brokers) == 0 {
		return nil
	}
	conn, err := kafkago.DialContext(ctx, "tcp", b.brokers[0])
	if err != nil {
		return fmt.Errorf("kafka: dial: %w", err)
	}
	defer conn.Close()

	ctrl, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("kafka: controller: %w", err)
	}
	ctrlConn, err := kafkago.DialContext(ctx, "tcp", net.JoinHostPort(ctrl.Host, fmt.Sprint(ctrl.Port)))
	if err != nil {
		return fmt.Errorf("kafka: dial controller: %w", err)
	}
	defer ctrlConn.Close()

	err = ctrlConn.CreateTopics(kafkago.TopicConfig{
		Topic:             topic,
		NumPartitions:     b.cfg.NumPartitions,
		ReplicationFactor: b.cfg.ReplicationFactor,
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("kafka: create topic %q: %w", topic, err)
	}
	return nil
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
