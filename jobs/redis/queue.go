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
	"os"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/jobs"
)

const (
	defaultStream         = "maniflex:jobs"
	defaultGroup          = "workers"
	defaultBlockDuration  = 200 * time.Millisecond
	delayedSetSuffix      = ":delayed"
	promoterInterval      = 500 * time.Millisecond
	defaultReclaimMinIdle = 5 * time.Minute
)

type pendingEntry struct {
	msgID string
	job   jobs.Job
}

// Queue is both a jobs.Queue (producer) and a jobs.Source (consumer).
type Queue struct {
	client         *goredis.Client
	ops            streamOps
	stream         string
	delayed        string // ZSET key for delayed jobs
	group          string
	consumerID     string
	reclaimMinIdle time.Duration

	// pending maps job.ID → pendingEntry so Ack/Nack/RenewLease can address the
	// stream message. It is per-process and lost on crash — which is exactly why
	// recovery cannot rely on it, and the reclaimer works off the Redis PEL.
	pendingMu sync.Mutex
	pending   map[string]pendingEntry

	promoterOnce sync.Once
	promoterStop chan struct{}
}

// Options configures the Redis Queue.
type Options struct {
	// Stream is the Redis stream key. Default: "maniflex:jobs".
	Stream string
	// Group is the consumer group name. Default: "workers".
	Group string
	// ConsumerID uniquely identifies this worker within the group. Default:
	// "maniflex-{hostname}-{pid}". It MUST be unique per running worker: two
	// workers sharing a ConsumerID are one consumer to Redis, sharing a single
	// pending list, which defeats per-worker crash recovery. Set it explicitly
	// to a stable per-worker value in production if you want a crashed worker to
	// reclaim its own pending entries by name on restart; otherwise another
	// worker's reclaimer recovers them regardless.
	ConsumerID string
	// ReclaimMinIdle is how long a delivered-but-unacknowledged message must sit
	// idle before another worker may reclaim it. Default: 5m.
	//
	// It is a crash-detection window, not a job-duration limit: a live worker
	// renews its hold every LeaseRenew (via RenewLease), so a job running longer
	// than this is not reclaimed. Only a worker that has stopped renewing — one
	// that crashed or hung — lets its jobs age past the threshold.
	ReclaimMinIdle time.Duration
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
		opts.ConsumerID = defaultConsumerName()
	}
	if opts.ReclaimMinIdle <= 0 {
		opts.ReclaimMinIdle = defaultReclaimMinIdle
	}
	return &Queue{
		client:         client,
		ops:            redisStreamOps{client: client},
		stream:         opts.Stream,
		delayed:        opts.Stream + delayedSetSuffix,
		group:          opts.Group,
		consumerID:     opts.ConsumerID,
		reclaimMinIdle: opts.ReclaimMinIdle,
		pending:        make(map[string]pendingEntry),
		promoterStop:   make(chan struct{}),
	}
}

// defaultConsumerName identifies this process uniquely within the group. A pod
// keeps its hostname across container restarts, so a restarted worker reclaims
// its own pending entries by name; the pid disambiguates multiple workers in
// one host. Redis never removes consumers from a group, so a name that changes
// every start slowly accretes empty consumer entries — set Options.ConsumerID
// to a stable per-worker value to avoid that.
func defaultConsumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("maniflex-%s-%d", host, os.Getpid())
}

// EnsureGroup creates the consumer group on the stream (MKSTREAM). Safe to
// call repeatedly; BUSYGROUP errors are silenced.
func (q *Queue) EnsureGroup(ctx context.Context) error {
	if err := q.ops.EnsureGroup(ctx, q.stream, q.group); err != nil {
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

// Dequeue claims up to n jobs. It first reclaims messages abandoned by a
// crashed worker, then reads new ones; either way it blocks at most ~200ms
// when idle.
//
// Before this, Dequeue read only with XREADGROUP ">", which returns solely
// never-delivered messages. A worker that died after delivery but before
// Ack/Nack left its message in the group's pending entries list (PEL), where
// nothing ever redelivered it — the job was lost while the queue looked
// healthy, and the in-memory pending map that Ack/Nack relies on had been lost
// with the process (audit JB-3). The reclaim step recovers those via
// XAUTOCLAIM once they have been idle past ReclaimMinIdle.
func (q *Queue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	// Reclaim first, so a crashed worker's backlog drains ahead of new work.
	// A reclaim error is not fatal to the call: fall through and still service
	// new messages rather than stalling the whole queue on a transient hiccup.
	if reclaimed := q.reclaim(ctx, n); len(reclaimed) > 0 {
		return reclaimed, nil
	}

	streams, err := q.ops.ReadGroup(ctx, q.stream, q.group, q.consumerID, int64(n), defaultBlockDuration)
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
		out = append(out, q.collect(ctx, s.Messages)...)
	}
	return out, nil
}

// reclaim takes up to n messages that have been pending longer than
// ReclaimMinIdle and returns them as jobs, tracked for Ack/Nack exactly like a
// fresh delivery. Starting the scan from "0-0" each call is deliberate: a
// reclaimed message's idle clock resets to now, so it falls out of the next
// scan's window without a persisted cursor.
func (q *Queue) reclaim(ctx context.Context, n int) []jobs.Job {
	msgs, _, err := q.ops.AutoClaim(ctx, q.stream, q.group, q.consumerID, q.reclaimMinIdle, "0-0", int64(n))
	if err != nil {
		return nil // transient; the next Dequeue tries again
	}
	return q.collect(ctx, msgs)
}

// collect turns claimed stream messages into jobs, acking and dropping any that
// will not decode (a retry would decode the same bytes and fail identically),
// and recording each in the pending map so Ack/Nack/RenewLease can address it.
func (q *Queue) collect(ctx context.Context, msgs []goredis.XMessage) []jobs.Job {
	var out []jobs.Job
	for _, msg := range msgs {
		raw, _ := msg.Values["job"].(string)
		if raw == "" {
			continue
		}
		var j jobs.Job
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			_ = q.ops.Ack(ctx, q.stream, q.group, msg.ID)
			continue
		}
		j.Attempts++
		q.pendingMu.Lock()
		q.pending[j.ID] = pendingEntry{msgID: msg.ID, job: j}
		q.pendingMu.Unlock()
		out = append(out, j)
	}
	return out
}

// RenewLease resets the idle clock on the running job's stream message, so a
// long-running job is not mistaken for one abandoned by a crashed worker and
// reclaimed out from under its live handler. The worker calls it periodically
// (WorkerConfig.LeaseRenew) for any Source implementing jobs.LeaseRenewer.
//
// d is ignored: a Redis stream has no per-message lease duration, only an idle
// clock, and reclaim eligibility is governed by ReclaimMinIdle. Renewing simply
// resets that clock to zero.
func (q *Queue) RenewLease(ctx context.Context, id string, _ time.Duration) error {
	q.pendingMu.Lock()
	e, ok := q.pending[id]
	q.pendingMu.Unlock()
	if !ok || e.msgID == "" {
		return nil
	}
	if _, err := q.ops.Claim(ctx, q.stream, q.group, q.consumerID, []string{e.msgID}); err != nil {
		return fmt.Errorf("jobs/redis: renew lease: %w", err)
	}
	return nil
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
	if err := q.ops.Ack(ctx, q.stream, q.group, e.msgID); err != nil {
		return fmt.Errorf("jobs/redis: nack xack: %w", err)
	}
	_, err := q.EnqueueAt(ctx, e.job, time.Now().Add(delay))
	return err
}

func (q *Queue) Dead(ctx context.Context, id string, jobErr error) error {
	return q.xack(ctx, id)
}

// Requeue returns j to the queue without spending a retry attempt: it acks the
// current stream message and re-enqueues j (with the Worker's header changes)
// to run again after delay. Unlike Nack it does not carry retry semantics — the
// Worker uses it for a job of a type this worker cannot handle, so another can
// (audit JB-4/JB-9). j.Attempts is stored as given; the next Dequeue increments
// it, so an unhandled round-trip leaves the effective attempt count unchanged.
func (q *Queue) Requeue(ctx context.Context, j jobs.Job, delay time.Duration) error {
	q.pendingMu.Lock()
	e, ok := q.pending[j.ID]
	delete(q.pending, j.ID)
	q.pendingMu.Unlock()
	if ok && e.msgID != "" {
		if err := q.ops.Ack(ctx, q.stream, q.group, e.msgID); err != nil {
			return fmt.Errorf("jobs/redis: requeue ack: %w", err)
		}
	}
	if _, err := q.EnqueueAt(ctx, j, time.Now().Add(delay)); err != nil {
		return fmt.Errorf("jobs/redis: requeue: %w", err)
	}
	return nil
}

func (q *Queue) xack(ctx context.Context, jobID string) error {
	q.pendingMu.Lock()
	e, ok := q.pending[jobID]
	delete(q.pending, jobID)
	q.pendingMu.Unlock()
	if !ok || e.msgID == "" {
		return nil
	}
	return q.ops.Ack(ctx, q.stream, q.group, e.msgID)
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
	_ jobs.Queue        = (*Queue)(nil)
	_ jobs.Source       = (*Queue)(nil)
	_ jobs.LeaseRenewer = (*Queue)(nil)
	_ jobs.Requeuer     = (*Queue)(nil)
)
