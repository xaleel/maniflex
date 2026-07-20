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
//
// # Retention
//
// Each stream is capped at Options.MaxLen entries (default 100,000). Streams
// are not queues — an entry stays after it is read — so an uncapped stream
// grows until Redis runs out of memory.
//
// The cap costs events rather than memory. Trimming deletes the oldest entries
// and does not consult consumer groups, so an entry a consumer has read but not
// yet acknowledged is dropped along with the rest: no error reaches the
// publisher and no consumer learns it existed. Size MaxLen against how far
// behind you are willing to let a consumer fall, not against throughput, or
// set MaxLenUnlimited and bound growth some other way.
//
// Each event is written to two streams — its own and the hub — in one
// MULTI/EXEC transaction, so the two cannot disagree about whether it happened.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

	// defaultMaxLen caps each stream. Streams are not queues: entries stay
	// after they are read, so an uncapped stream grows for as long as the
	// process publishes and eventually exhausts Redis's memory.
	defaultMaxLen = 100_000
)

// MaxLenUnlimited disables stream trimming when assigned to Options.MaxLen.
//
// Nothing then bounds stream growth, so pair it with a Redis `maxmemory`
// policy or an external trim. It exists because the alternative — trimming —
// deletes events, and which of the two is acceptable is not a decision this
// package can make for you.
const MaxLenUnlimited = -1

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

	// MaxLen caps the number of entries retained per stream. Default: 100_000.
	// Set MaxLenUnlimited to disable trimming.
	//
	// Trimming drops the OLDEST entries, and it does not consult consumer
	// groups: an entry still pending in a group's PEL — read but not yet
	// acknowledged — is deleted along with everything else that aged out. The
	// event is then gone, no error is returned to the publisher, and no
	// consumer learns it existed. This is the failure mode of a consumer that
	// falls more than MaxLen behind, so size it against how far behind you are
	// willing to let one fall rather than against steady-state throughput.
	//
	// Trimming is approximate (Redis's `~`), which trims at radix-tree node
	// boundaries: the stream retains at least MaxLen, usually somewhat more.
	// It is much cheaper than exact trimming and errs toward keeping data.
	MaxLen int64
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
	// Only the zero value takes the default: a negative MaxLen is an explicit
	// MaxLenUnlimited and must survive, which is why this is not the `<= 0`
	// test the options above use.
	if o.MaxLen == 0 {
		o.MaxLen = defaultMaxLen
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

// xaddArgs builds the XADD for one stream, applying the configured trim.
//
// A MaxLen of zero leaves XAddArgs.MaxLen unset, which omits the MAXLEN clause
// entirely — that is how MaxLenUnlimited takes effect.
func (b *Bus) xaddArgs(stream string, values map[string]any) *goredis.XAddArgs {
	args := &goredis.XAddArgs{Stream: stream, Values: values}
	if b.opts.MaxLen > 0 {
		args.MaxLen = b.opts.MaxLen
		args.Approx = true
	}
	return args
}

// streams returns the two streams every event is written to: its own typed
// stream, and the hub that wildcard subscribers read.
func (b *Bus) streams(eventType string) []string {
	return []string{b.key(eventType), b.hubKey()}
}

// Publish adds e to its typed stream and the hub stream.
//
// Both writes go out as one MULTI/EXEC transaction. A plain pipeline only
// batches — it does not make the writes atomic — so a connection lost between
// them left the event on one stream and not the other, and the two subscriber
// kinds disagreed permanently about whether it happened (audit EV-14).
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("redis: marshal: %w", err)
	}
	values := map[string]any{"id": e.ID, "payload": string(payload)}

	pipe := b.client.TxPipeline()
	for _, stream := range b.streams(e.Type) {
		pipe.XAdd(ctx, b.xaddArgs(stream, values))
	}
	_, err = pipe.Exec(ctx)
	return err
}

// PublishBatch publishes all events in a single transaction, so a batch either
// lands whole or not at all — including each event's typed/hub pair.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	if len(es) == 0 {
		return nil
	}
	pipe := b.client.TxPipeline()
	for _, e := range es {
		// The error was discarded here, publishing an empty payload that no
		// subscriber can decode — a poison entry manufactured out of a
		// reportable failure. Publish always checked; this did not.
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("redis: marshal event %q: %w", e.ID, err)
		}
		values := map[string]any{"id": e.ID, "payload": string(payload)}
		for _, stream := range b.streams(e.Type) {
			pipe.XAdd(ctx, b.xaddArgs(stream, values))
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

	// A read loop retries forever — stopping would silently end consumption —
	// so the pacing is what keeps that affordable. The previous fixed
	// time.Sleep(time.Second) never grew, never jittered, and ignored ctx
	// (audit EV-13).
	var bo events.ReadBackoff

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		results, err := b.ops.ReadGroup(ctx, stream, sub.Group, consumer,
			int64(sub.Concurrency*2), time.Second)
		if err != nil {
			// goredis.Nil is the block timeout expiring with nothing to read.
			// That is the idle steady state, not a failure: it must not consume
			// an attempt or the backoff would grow on a healthy but quiet stream.
			if err == goredis.Nil || ctx.Err() != nil {
				continue
			}
			attempt, delay, escalate := bo.Next()
			logReadFailure(stream, attempt, delay, escalate, err)
			if !bo.Wait(ctx, delay) {
				return nil
			}
			continue
		}
		bo.Reset()

		for _, result := range results {
			b.dispatch(ctx, stream, sub, result.Messages, sem, &wg)
		}
	}
}

// logReadFailure reports a failed stream read. Before audit EV-13 this loop
// was entirely silent: a consumer that could not reach Redis looked identical
// to one on an idle stream. escalate is true exactly once, when the backoff
// first reaches its ceiling, marking the point where a run of failures stopped
// being a blip — later retries stay at WARN so a long outage does not bury the
// logs in ERRORs.
func logReadFailure(stream string, attempt int, delay time.Duration, escalate bool, err error) {
	attrs := []any{
		slog.String("stream", stream),
		slog.Int("attempt", attempt),
		slog.Duration("retry_in", delay),
		slog.String("error", err.Error()),
	}
	if escalate {
		slog.Default().Error("events: redis read failing persistently, consumer is not progressing", attrs...)
		return
	}
	slog.Default().Warn("events: redis read failed, retrying", attrs...)
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
