// Package redis provides a Redis Streams job queue adapter for maniflex jobs.
//
// Jobs are published to a Redis Stream (XADD). A consumer group (XREADGROUP)
// ensures at-least-once delivery across multiple workers. Delayed jobs land
// in a sorted set and are moved to the stream by a background promoter
// goroutine.
//
// This adapter does not provide transactional outbox semantics — enqueues are
// best-effort after the surrounding business write. Use jobs/sql when
// atomic outbox guarantees are required.
package redis

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"maniflex/jobs"
)

const (
	defaultStream        = "maniflex:jobs"
	defaultGroup         = "workers"
	defaultConsumerID    = "worker-0"
	defaultBlockDuration = 200 * time.Millisecond
	delayedSetSuffix     = ":delayed"
	promoterInterval     = 500 * time.Millisecond
)

type pendingEntry struct {
	msgID string
	job   jobs.Job
}

// Queue is both a jobs.Queue (producer) and a jobs.Source (consumer).
type Queue struct {
	client     *goredis.Client
	stream     string
	delayed    string // ZSET key for delayed jobs
	group      string
	consumerID string

	// pending maps job.ID → pendingEntry so Ack/Nack can XACK and re-enqueue.
	pendingMu  sync.Mutex
	pending    map[string]pendingEntry

	promoterOnce sync.Once
	promoterStop chan struct{}
}

// Options configures the Redis Queue.
type Options struct {
	// Stream is the Redis stream key. Default: "maniflex:jobs".
	Stream string
	// Group is the consumer group name. Default: "workers".
	Group string
	// ConsumerID uniquely identifies this worker within the group.
	// Default: "worker-0". Use a hostname/pod-name in production.
	ConsumerID string
}

// New creates a Queue backed by client. Call EnsureGroup before using it as a Source.
func New(client *goredis.Client, opts Options) *Queue {
	if opts.Stream == "" {
		opts.Stream = defaultStream
	}
	if opts.Group == "" {
		opts.Group = defaultGroup
	}
	if opts.ConsumerID == "" {
		opts.ConsumerID = defaultConsumerID
	}
	return &Queue{
		client:       client,
		stream:       opts.Stream,
		delayed:      opts.Stream + delayedSetSuffix,
		group:        opts.Group,
		consumerID:   opts.ConsumerID,
		pending:      make(map[string]pendingEntry),
		promoterStop: make(chan struct{}),
	}
}

// EnsureGroup creates the consumer group on the stream (MKSTREAM). Safe to
// call repeatedly; BUSYGROUP errors are silenced.
func (q *Queue) EnsureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("jobs/redis: EnsureGroup: %w", err)
	}
	return nil
}

// StartPromoter launches a background goroutine that moves due delayed jobs
// from the sorted set to the stream. Call once after EnsureGroup.
func (q *Queue) StartPromoter(ctx context.Context) {
	q.promoterOnce.Do(func() {
		go q.promoteLoop(ctx)
	})
}

// Close stops the promoter goroutine.
func (q *Queue) Close() error {
	select {
	case <-q.promoterStop:
	default:
		close(q.promoterStop)
	}
	return nil
}

// ── Queue (producer) ──────────────────────────────────────────────────────────

func (q *Queue) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	return q.enqueueAt(ctx, j, time.Time{})
}

func (q *Queue) EnqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	return q.enqueueAt(ctx, j, at)
}

func (q *Queue) EnqueueBatch(ctx context.Context, js []jobs.Job) ([]string, error) {
	ids := make([]string, len(js))
	for i, j := range js {
		id, err := q.Enqueue(ctx, j)
		if err != nil {
			return ids, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (q *Queue) enqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	if j.ID == "" {
		j.ID = newID()
	}
	if j.MaxRetry == 0 {
		j.MaxRetry = 3
	}
	data, err := json.Marshal(j)
	if err != nil {
		return "", fmt.Errorf("jobs/redis: marshal: %w", err)
	}

	// Delayed: push to ZSET with Unix-milli score.
	if !at.IsZero() && at.After(time.Now()) {
		if err := q.client.ZAdd(ctx, q.delayed, goredis.Z{
			Score:  float64(at.UnixMilli()),
			Member: string(data),
		}).Err(); err != nil {
			return "", fmt.Errorf("jobs/redis: zadd delayed: %w", err)
		}
		return j.ID, nil
	}

	// Immediate: XADD to the stream.
	if err := q.client.XAdd(ctx, &goredis.XAddArgs{
		Stream: q.stream,
		ID:     "*",
		Values: map[string]any{"job": string(data)},
	}).Err(); err != nil {
		return "", fmt.Errorf("jobs/redis: xadd: %w", err)
	}
	return j.ID, nil
}

// ── Source (consumer) ─────────────────────────────────────────────────────────

// Dequeue claims up to n jobs via XREADGROUP. Blocks up to 200ms before
// returning an empty slice when the stream is idle.
func (q *Queue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	streams, err := q.client.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group:    q.group,
		Consumer: q.consumerID,
		Streams:  []string{q.stream, ">"},
		Count:    int64(n),
		Block:    defaultBlockDuration,
		NoAck:    false,
	}).Result()
	if err != nil {
		if err == goredis.Nil || isNoGroup(err) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("jobs/redis: xreadgroup: %w", err)
	}

	var out []jobs.Job
	for _, s := range streams {
		for _, msg := range s.Messages {
			raw, _ := msg.Values["job"].(string)
			if raw == "" {
				continue
			}
			var j jobs.Job
			if err := json.Unmarshal([]byte(raw), &j); err != nil {
				// Malformed message: ack and skip.
				_ = q.client.XAck(ctx, q.stream, q.group, msg.ID).Err()
				continue
			}
			j.Attempts++
			// Track the stream message ID and job for Ack/Nack.
			q.pendingMu.Lock()
			q.pending[j.ID] = pendingEntry{msgID: msg.ID, job: j}
			q.pendingMu.Unlock()
			out = append(out, j)
		}
	}
	return out, nil
}

func (q *Queue) Ack(ctx context.Context, id string) error {
	return q.xack(ctx, id)
}

// Nack removes the message from the PEL and re-enqueues the job after delay.
func (q *Queue) Nack(ctx context.Context, id string, jobErr error, delay time.Duration) error {
	q.pendingMu.Lock()
	e, ok := q.pending[id]
	delete(q.pending, id)
	q.pendingMu.Unlock()
	if !ok {
		return nil
	}
	if err := q.client.XAck(ctx, q.stream, q.group, e.msgID).Err(); err != nil {
		return fmt.Errorf("jobs/redis: nack xack: %w", err)
	}
	_, err := q.EnqueueAt(ctx, e.job, time.Now().Add(delay))
	return err
}

func (q *Queue) Dead(ctx context.Context, id string, jobErr error) error {
	return q.xack(ctx, id)
}

func (q *Queue) xack(ctx context.Context, jobID string) error {
	q.pendingMu.Lock()
	e, ok := q.pending[jobID]
	delete(q.pending, jobID)
	q.pendingMu.Unlock()
	if !ok || e.msgID == "" {
		return nil
	}
	return q.client.XAck(ctx, q.stream, q.group, e.msgID).Err()
}

// ── promoter ──────────────────────────────────────────────────────────────────

func (q *Queue) promoteLoop(ctx context.Context) {
	ticker := time.NewTicker(promoterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.promoterStop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.promoteDue(ctx)
		}
	}
}

func (q *Queue) promoteDue(ctx context.Context) {
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	members, err := q.client.ZRangeByScore(ctx, q.delayed, &goredis.ZRangeBy{
		Min: "-inf", Max: now, Count: 100,
	}).Result()
	if err != nil || len(members) == 0 {
		return
	}
	pipe := q.client.Pipeline()
	for _, m := range members {
		pipe.XAdd(ctx, &goredis.XAddArgs{
			Stream: q.stream,
			ID:     "*",
			Values: map[string]any{"job": m},
		})
		pipe.ZRem(ctx, q.delayed, m)
	}
	_, _ = pipe.Exec(ctx)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func isNoGroup(err error) bool {
	s := err.Error()
	return len(s) >= 7 && s[:7] == "NOGROUP"
}

func newID() string {
	var rnd [10]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%013x%x", time.Now().UnixMilli(), rnd)
}

// Compile-time interface checks.
var (
	_ jobs.Queue  = (*Queue)(nil)
	_ jobs.Source = (*Queue)(nil)
)
