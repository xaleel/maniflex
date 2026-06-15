// Package redis provides a Redis Streams event bus adapter for maniflex events.
//
// Events are published to individual streams keyed by event Type
// (e.g. "myapp:invoice.created"). A hub stream ("myapp:*") receives every event
// for subscriptions that use wildcard or multi-type patterns, enabling client-side
// pattern filtering without requiring SCAN on publish.
//
// Consumer groups (XREADGROUP) provide at-least-once delivery.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"maniflex/events"
)

// Bus is a Redis Streams event bus.
type Bus struct {
	client *goredis.Client
	prefix string
}

// New creates a Redis Streams Bus. prefix is prepended to all stream keys,
// e.g. "hospital" → "hospital:invoice.created" and "hospital:*" (hub).
func New(client *goredis.Client, prefix string) *Bus {
	return &Bus{client: client, prefix: prefix}
}

func (b *Bus) key(eventType string) string {
	if b.prefix != "" {
		return b.prefix + ":" + eventType
	}
	return eventType
}

// hubKey is the fan-out stream that receives every event for wildcard consumers.
func (b *Bus) hubKey() string { return b.key("*") }

// Publish adds e to its typed stream and the hub stream.
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("redis: marshal: %w", err)
	}
	values := map[string]any{"id": e.ID, "payload": string(payload)}

	pipe := b.client.Pipeline()
	for _, stream := range []string{b.key(e.Type), b.hubKey()} {
		pipe.XAdd(ctx, &goredis.XAddArgs{
			Stream: stream,
			MaxLen: 100_000,
			Approx: true,
			Values: values,
		})
	}
	_, err = pipe.Exec(ctx)
	return err
}

// PublishBatch publishes all events in a single pipelined command.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	if len(es) == 0 {
		return nil
	}
	pipe := b.client.Pipeline()
	for _, e := range es {
		payload, _ := json.Marshal(e)
		values := map[string]any{"id": e.ID, "payload": string(payload)}
		for _, stream := range []string{b.key(e.Type), b.hubKey()} {
			pipe.XAdd(ctx, &goredis.XAddArgs{
				Stream: stream,
				MaxLen: 100_000,
				Approx: true,
				Values: values,
			})
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Subscribe starts a consumer group reader for each pattern and delivers
// matching events to sub.Handler. Exact patterns read from the typed stream;
// wildcard patterns read from the hub stream.
//
// Returns a Cancel function that stops all goroutines for this subscription.
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
		var stream string
		if isWildcard(pattern) {
			stream = b.hubKey()
		} else {
			stream = b.key(pattern)
		}
		wg.Go(func() {
			_ = b.runConsumer(cctx, stream, sub)
		})
	}

	return func() {
		cancel()
		wg.Wait()
	}, nil
}

func (b *Bus) runConsumer(ctx context.Context, stream string, sub events.Subscription) error {
	// Create the consumer group (idempotent).
	_ = b.client.XGroupCreateMkStream(ctx, stream, sub.Group, "0").Err()

	consumer := fmt.Sprintf("worker-%d", time.Now().UnixNano())
	sem := make(chan struct{}, sub.Concurrency)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		default:
		}

		results, err := b.client.XReadGroup(ctx, &goredis.XReadGroupArgs{
			Group:    sub.Group,
			Consumer: consumer,
			Streams:  []string{stream, ">"},
			Count:    int64(sub.Concurrency * 2),
			Block:    time.Second,
		}).Result()
		if err != nil {
			if err == goredis.Nil || ctx.Err() != nil {
				continue
			}
			time.Sleep(time.Second)
			continue
		}

		for _, result := range results {
			for _, msg := range result.Messages {
				msgID := msg.ID
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() {
						<-sem
						wg.Done()
					}()
					payload, _ := msg.Values["payload"].(string)
					var e events.Event
					if err := json.Unmarshal([]byte(payload), &e); err != nil {
						b.client.XAck(ctx, stream, sub.Group, msgID)
						return
					}
					if !matchesAny(sub.Patterns, e.Type) {
						b.client.XAck(ctx, stream, sub.Group, msgID)
						return
					}
					events.DeliverWithRetry(ctx, b, sub, e)
					b.client.XAck(ctx, stream, sub.Group, msgID)
				}()
			}
		}
	}
}

// Close is a no-op; close the *goredis.Client directly.
func (b *Bus) Close() error { return nil }

func isWildcard(pattern string) bool {
	return pattern == "*" || len(pattern) > 0 && (pattern[len(pattern)-1] == '*' || contains(pattern, '*'))
}

func contains(s string, c byte) bool {
	for i := range len(s) {
		if s[i] == c {
			return true
		}
	}
	return false
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

