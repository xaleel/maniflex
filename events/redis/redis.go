// Package redis provides a Redis Streams event bus adapter for maniflex events.
//
// Events are published to individual streams keyed by event Type
// (e.g. "myapp:invoice.created"). A hub stream ("myapp:*") receives every event
// for subscriptions that use wildcard or multi-type patterns, enabling client-side
// pattern filtering without requiring SCAN on publish.
//
// Consumer groups (XREADGROUP) provide at-least-once delivery.
//
// # Recovery
//
// XREADGROUP's ">" returns only never-delivered messages, so a consumer that
// dies mid-delivery leaves its messages in the group's pending list where
// nothing would redeliver them. Each consumer therefore also runs a periodic
// XAUTOCLAIM sweep, taking over messages idle longer than Options.ClaimMinIdle.
//
// That threshold is the one setting worth tuning: it must exceed your slowest
// healthy handler including retries, because a message becomes claimable while
// its original consumer may still be working on it, and claiming early means
// delivering twice.
//
// Options.ConsumerName must be stable across restarts — Redis never removes
// consumers from a group, so a name that changes each start adds a permanent
// entry every deploy. The default derives from hostname and pid.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/events"
)

// Default reclaim tuning. ClaimMinIdle must comfortably exceed the slowest
// healthy handler, or a live consumer's work is stolen while it is still
// running and the event is delivered twice.
const (
	defaultClaimMinIdle  = 5 * time.Minute
	defaultClaimInterval = 30 * time.Second
)

// Options customises the Bus. All fields are optional.
type Options struct {
	// ConsumerName identifies this process within every consumer group.
	// Default: "maniflex-{hostname}-{pid}".
	//
	// It must be stable across restarts of the same logical worker. Redis never
	// removes consumers from a group, so a name containing a timestamp — which
	// this adapter used to generate — adds a permanent entry on every deploy and
	// leaves that entry holding any messages the process had in flight.
	ConsumerName string

	// ClaimMinIdle is how long a message must sit unacknowledged before another
	// consumer may claim it. Default: 5m.
	//
	// Set this above the slowest handler you expect, including its retries: a
	// message is claimable while its original consumer is still working on it,
	// and claiming early means delivering twice.
	ClaimMinIdle time.Duration

	// ClaimInterval is how often to scan for reclaimable messages.
	// Default: 30s.
	ClaimInterval time.Duration
}

// Bus is a Redis Streams event bus.
type Bus struct {
	client *goredis.Client
	ops    streamOps
	prefix string
	opts   Options
}

// New creates a Redis Streams Bus. prefix is prepended to all stream keys,
// e.g. "hospital" → "hospital:invoice.created" and "hospital:*" (hub).
//
// Consumers periodically reclaim messages abandoned by a consumer that died
// mid-delivery; see Options.ClaimMinIdle.
func New(client *goredis.Client, prefix string, opts ...Options) *Bus {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.ConsumerName == "" {
		o.ConsumerName = defaultConsumerName()
	}
	if o.ClaimMinIdle <= 0 {
		o.ClaimMinIdle = defaultClaimMinIdle
	}
	if o.ClaimInterval <= 0 {
		o.ClaimInterval = defaultClaimInterval
	}
	return &Bus{
		client: client,
		ops:    redisStreamOps{client: client},
		prefix: prefix,
		opts:   o,
	}
}

// defaultConsumerName identifies this process in a way that survives a restart
// under an orchestrator: a pod keeps its hostname across container restarts, so
// the same worker reclaims its own pending messages rather than orphaning them
// under a name that never returns.
func defaultConsumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("maniflex-%s-%d", host, os.Getpid())
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
	_ = b.ops.EnsureGroup(ctx, stream, sub.Group)

	consumer := b.opts.ConsumerName
	sem := make(chan struct{}, sub.Concurrency)
	var wg sync.WaitGroup

	// Reclaim messages abandoned by a consumer that died mid-delivery. Without
	// it, XREADGROUP's ">" only ever returns never-delivered messages, so a
	// crashed consumer's pending entries sat in the group's PEL forever —
	// unacknowledged, never redelivered, effectively lost while looking
	// perfectly healthy (audit EV-7).
	claimDone := make(chan struct{})
	go func() {
		defer close(claimDone)
		b.runReclaimer(ctx, stream, sub, consumer, sem, &wg)
	}()

	defer func() {
		<-claimDone
		wg.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		results, err := b.ops.ReadGroup(ctx, stream, sub.Group, consumer,
			int64(sub.Concurrency*2), time.Second)
		if err != nil {
			if err == goredis.Nil || ctx.Err() != nil {
				continue
			}
			time.Sleep(time.Second)
			continue
		}

		for _, result := range results {
			b.dispatch(ctx, stream, sub, result.Messages, sem, &wg)
		}
	}
}

// runReclaimer periodically claims messages that have been pending longer than
// ClaimMinIdle and delivers them like any other, until ctx ends.
func (b *Bus) runReclaimer(ctx context.Context, stream string, sub events.Subscription,
	consumer string, sem chan struct{}, wg *sync.WaitGroup,
) {
	ticker := time.NewTicker(b.opts.ClaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		b.reclaimOnce(ctx, stream, sub, consumer, sem, wg)
	}
}

// reclaimOnce walks one full XAUTOCLAIM scan and dispatches everything it
// claims. The scan is cursor-paged: each call returns the cursor to resume
// from, and "0-0" means it wrapped.
func (b *Bus) reclaimOnce(ctx context.Context, stream string, sub events.Subscription,
	consumer string, sem chan struct{}, wg *sync.WaitGroup,
) {
	start := "0-0"
	// Bounded rather than "until the cursor wraps": the cursor comes from the
	// server, and a loop whose only exit depends on a remote value is one
	// malformed reply away from spinning forever inside a goroutine nobody is
	// watching. A scan that does not finish here resumes on the next tick.
	for range maxClaimPages {
		if ctx.Err() != nil {
			return
		}
		msgs, next, err := b.ops.AutoClaim(ctx, stream, sub.Group, consumer,
			b.opts.ClaimMinIdle, start, claimBatchSize)
		if err != nil {
			return // transient; the next tick tries again
		}
		b.dispatch(ctx, stream, sub, msgs, sem, wg)
		if next == "" || next == "0-0" {
			return // scan complete
		}
		start = next
	}
}

const (
	// claimBatchSize bounds one XAUTOCLAIM call.
	claimBatchSize = 100
	// maxClaimPages bounds one reclaim sweep, so a backlog is drained across
	// ticks instead of monopolising the goroutine.
	maxClaimPages = 20
)

// dispatch hands each message to the handler on the bounded worker pool, acking
// it once delivery has been attempted. Shared by the read and reclaim paths so
// a claimed message is treated exactly like a freshly delivered one — including
// being acked, without which it would be reclaimed again on every sweep.
func (b *Bus) dispatch(ctx context.Context, stream string, sub events.Subscription,
	msgs []goredis.XMessage, sem chan struct{}, wg *sync.WaitGroup,
) {
	for _, msg := range msgs {
		msgID := msg.ID
		payload, _ := msg.Values["payload"].(string)

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			var e events.Event
			if err := json.Unmarshal([]byte(payload), &e); err != nil {
				_ = b.ops.Ack(ctx, stream, sub.Group, msgID)
				return
			}
			if !matchesAny(sub.Patterns, e.Type) {
				_ = b.ops.Ack(ctx, stream, sub.Group, msgID)
				return
			}
			events.DeliverWithRetry(ctx, b, sub, e)
			_ = b.ops.Ack(ctx, stream, sub.Group, msgID)
		}()
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

