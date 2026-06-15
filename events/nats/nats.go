// Package nats provides a NATS JetStream event bus adapter for maniflex events.
//
// Events are published to JetStream subjects derived from the event Type.
// Durable consumer groups map to JetStream durable consumers, providing
// at-least-once delivery with automatic retry and dead-letter support.
package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	natsclient "github.com/nats-io/nats.go"

	"github.com/xaleel/maniflex/events"
)

// Bus is a NATS JetStream event bus.
type Bus struct {
	nc     *natsclient.Conn
	js     natsclient.JetStreamContext
	stream string // JetStream stream name
}

// New creates a NATS JetStream Bus. stream is the JetStream stream name that
// must already exist (or be created via New with auto-create).
// subject prefix uses dot notation: event Type "invoice.created" maps to
// subject "invoice.created" (subscribed as "invoice.>" for wildcard).
//
// The JetStream stream should be configured with subject filter ">":
//
//	js.AddStream(&nats.StreamConfig{
//	    Name:     "events",
//	    Subjects: []string{">"},
//	})
func New(nc *natsclient.Conn, stream string) (*Bus, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("nats: jetstream context: %w", err)
	}
	return &Bus{nc: nc, js: js, stream: stream}, nil
}

// Publish publishes e to the JetStream subject derived from e.Type.
func (b *Bus) Publish(_ context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("nats: marshal: %w", err)
	}
	if _, err := b.js.Publish(e.Type, payload); err != nil {
		return fmt.Errorf("nats: publish: %w", err)
	}
	return nil
}

// PublishBatch publishes all events in es. Each publish is independent.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe creates a JetStream push-based durable consumer for each pattern
// in sub.Patterns and delivers matching events to sub.Handler.
// Pattern "invoice.*" maps to JetStream subject filter "invoice.*".
// Pattern "*" maps to ">".
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

	var (
		subs []*natsclient.Subscription
		mu   sync.Mutex
	)
	cctx, cancel := context.WithCancel(ctx)
	sem := make(chan struct{}, sub.Concurrency)

	for _, pattern := range sub.Patterns {
		subject := patternToSubject(pattern)
		durable := fmt.Sprintf("%s-%s", sub.Group, sanitise(subject))

		ns, err := b.js.Subscribe(subject, func(msg *natsclient.Msg) {
			var e events.Event
			if err := json.Unmarshal(msg.Data, &e); err != nil {
				msg.Ack()
				return
			}
			if !matchesAny(sub.Patterns, e.Type) {
				msg.Ack()
				return
			}
			// Acquire a concurrency slot in a goroutine so the NATS
			// dispatch callback returns immediately and is never blocked.
			go func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				events.DeliverWithRetry(cctx, b, sub, e)
				msg.Ack()
			}()
		},
			natsclient.Durable(durable),
			natsclient.AckExplicit(),
		)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("nats: subscribe %q: %w", subject, err)
		}
		mu.Lock()
		subs = append(subs, ns)
		mu.Unlock()
	}

	return func() {
		cancel()
		mu.Lock()
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
		mu.Unlock()
	}, nil
}

// Close drains and closes the NATS connection.
func (b *Bus) Close() error {
	b.nc.Drain()
	b.nc.Close()
	return nil
}

// patternToSubject converts a glob pattern to a JetStream subject filter.
// "*" → ">", "invoice.*" → "invoice.*", "invoice.created" → "invoice.created".
func patternToSubject(pattern string) string {
	if pattern == "*" {
		return ">"
	}
	// Replace trailing ".*" with ".>" for JetStream multi-level wildcard.
	if strings.HasSuffix(pattern, ".*") {
		return pattern[:len(pattern)-1] + ">"
	}
	return pattern
}

// sanitise removes characters invalid in NATS durable consumer names.
func sanitise(s string) string {
	return strings.NewReplacer(".", "_", ">", "all", "*", "any").Replace(s)
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

