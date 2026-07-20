// Package nats provides a NATS JetStream event bus adapter for maniflex events.
//
// Events are published to JetStream subjects derived from the event Type.
// Durable consumer groups map to JetStream durable consumers, providing
// at-least-once delivery with automatic retry and dead-letter support.
//
// # Consumer groups
//
// Subscription.Group becomes a JetStream queue group, so every replica running
// the same Group shares the work and each event is handled once by the group —
// the same meaning Group has in the Kafka and Redis adapters.
//
// # Durable names changed
//
// The durable consumer name is now "{group}-{subject}-{hash}". It was
// "{group}-{subject}", which was not unique: the rendering maps "." to "_" and
// ">" to "all", so "invoice.>" and "invoice.all" produced the same name, as did
// "a.b" and "a_b". A durable's filter subject is fixed when it is created, so
// the second of any colliding pair was refused with ErrSubjectMismatch.
//
// Both changes rename or reshape server-side state, so consumers created by an
// earlier version are not reused:
//
//   - The old durables still exist, holding their acknowledgement position, and
//     nothing consumes from them. Delete them once the new ones are running
//     (`nats consumer ls <stream>`, then `nats consumer rm`).
//   - New durables start at the stream's default delivery policy, so decide
//     whether a replay of retained events is acceptable before upgrading a
//     busy deployment, and drain the old consumers first if it is not.
package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	natsclient "github.com/nats-io/nats.go"

	"github.com/xaleel/maniflex/events"
)

// jsSubscription is the part of a live subscription this package uses.
type jsSubscription interface {
	Unsubscribe() error
}

// jsOps is the seam over the one JetStream call Subscribe makes.
//
// It exists to make the call's arguments assertable. The durable name and the
// queue group are otherwise buried in nats.go's SubOpt values, whose only
// method is unexported — so from outside that package there is no way to read
// back what was requested, and the two things most worth testing here are
// exactly which durable name and which queue group were used.
type jsOps interface {
	QueueSubscribe(subject, queue, durable string, cb natsclient.MsgHandler) (jsSubscription, error)
}

// jetStreamOps is the production jsOps, backed by a real JetStreamContext.
type jetStreamOps struct{ js natsclient.JetStreamContext }

func (o jetStreamOps) QueueSubscribe(subject, queue, durable string, cb natsclient.MsgHandler) (jsSubscription, error) {
	ns, err := o.js.QueueSubscribe(subject, queue, cb,
		natsclient.Durable(durable),
		natsclient.AckExplicit(),
	)
	if err != nil {
		return nil, err
	}
	return ns, nil
}

// Bus is a NATS JetStream event bus.
type Bus struct {
	nc     *natsclient.Conn
	js     natsclient.JetStreamContext
	ops    jsOps
	stream string // JetStream stream name
}

// New creates a NATS JetStream Bus. stream is the name of a JetStream stream
// that you must create yourself before using the bus — New does not create it
// (it only obtains a JetStreamContext). Subject prefix uses dot notation: event
// Type "invoice.created" maps to subject "invoice.created" (subscribed as
// "invoice.>" for wildcard).
//
// Scope the stream to the real business subjects you publish — NOT ">". A ">"
// filter captures NATS system subjects too, so the server only accepts it with
// NoAck:true, which is incompatible with this adapter's AckExplicit consumers
// (at-least-once delivery). A consumer with a ">" filter still binds fine to a
// scoped stream.
//
//	js.AddStream(&nats.StreamConfig{
//	    Name:     "events",
//	    Subjects: []string{"invoice.>", "order.>"}, // your namespaces, not ">"
//	})
func New(nc *natsclient.Conn, stream string) (*Bus, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("nats: jetstream context: %w", err)
	}
	return &Bus{nc: nc, js: js, ops: jetStreamOps{js: js}, stream: stream}, nil
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
		subs []jsSubscription
		mu   sync.Mutex
	)
	cctx, cancel := context.WithCancel(ctx)
	sem := make(chan struct{}, sub.Concurrency)

	// The queue group is what makes Group mean on NATS what it already means on
	// Kafka and Redis. A durable consumer with no deliver group accepts exactly
	// one bound subscription, so a second replica was refused outright with
	// "consumer is already bound to a subscription" (audit EV-15).
	queue := sanitise(sub.Group)

	for _, pattern := range sub.Patterns {
		subject := patternToSubject(pattern)
		durable := durableName(sub.Group, subject)

		ns, err := b.ops.QueueSubscribe(subject, queue, durable, func(msg *natsclient.Msg) {
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
		})
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

// maxReadableLen bounds the human-readable half of a durable name. The hash
// suffix is what guarantees uniqueness, so truncating the readable half costs
// legibility and nothing else.
const maxReadableLen = 40

// durableName builds the JetStream durable consumer name for one (group,
// subject) pair.
//
// The readable half is for whoever runs `nats consumer ls`; the hash is what
// makes the name unique. Both halves are needed: sanitise is many-to-one — it
// maps "." to "_" and ">" to "all", so the distinct subjects "invoice.>" and
// "invoice.all" both render as "invoice_all", as do "a.b" and "a_b" — and two
// subscriptions that collided got the second one refused by the server with
// ErrSubjectMismatch, because a durable's filter subject is fixed at creation
// (audit EV-15).
//
// The hash covers the group as well as the subject, since groups sanitise just
// as lossily as subjects do.
func durableName(group, subject string) string {
	// NUL separates the fields so that ("a", "b.c") and ("a.b", "c") cannot
	// hash to the same digest: it cannot occur in either input.
	sum := sha256.Sum256([]byte(group + "\x00" + subject))

	readable := sanitise(group) + "-" + sanitise(subject)
	if len(readable) > maxReadableLen {
		readable = readable[:maxReadableLen]
	}
	return readable + "-" + hex.EncodeToString(sum[:4])
}

// sanitise renders s for the readable half of a durable name.
//
// NATS rejects ".", "*" and ">" in durable and queue-group names. The wildcards
// are spelled out rather than dropped so the common case stays legible, and
// everything else outside [A-Za-z0-9_-] becomes "_" — the previous version
// replaced only three characters, which left any other invalid byte to be
// rejected by the server at Subscribe.
//
// This is deliberately lossy. Callers must not use it alone to identify a
// subject; see durableName.
func sanitise(s string) string {
	s = strings.NewReplacer(">", "all", "*", "any").Replace(s)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
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
