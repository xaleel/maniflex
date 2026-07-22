// Package inproc provides an in-process job queue backed by a goroutine pool.
// It is suitable for tests and single-binary applications. Jobs are lost if the
// process exits; use jobs/sql for durability.
package inproc

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// Queue is both a jobs.Queue (producer) and a jobs.Source (consumer).
// Create one with New and pass it to both the enqueue site and WorkerConfig.Source.
type Queue struct {
	mu           sync.Mutex
	entries      []*entry        // ordered list of pending/failed jobs
	byID         map[string]*entry
	runningKeys  map[string]int  // group_key → count of running jobs for that key
	closeCh      chan struct{}
	closeOnce    sync.Once
	notifyCh     chan struct{}    // pulsed (non-blocking) when new jobs arrive
}

type entry struct {
	job       jobs.Job
	status    jobs.Status
	attempts  int
	notBefore time.Time
}

// New returns a ready Queue.
func New() *Queue {
	return &Queue{
		byID:        make(map[string]*entry),
		runningKeys: make(map[string]int),
		closeCh:     make(chan struct{}),
		notifyCh:    make(chan struct{}, 1),
	}
}

// ── Queue (producer) ──────────────────────────────────────────────────────────

func (q *Queue) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	return q.enqueueAt(j, time.Now())
}

func (q *Queue) EnqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	return q.enqueueAt(j, at)
}

func (q *Queue) EnqueueBatch(ctx context.Context, js []jobs.Job) ([]string, error) {
	ids := make([]string, len(js))
	for i, j := range js {
		id, err := q.enqueueAt(j, time.Now())
		if err != nil {
			return ids, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (q *Queue) Close() error {
	q.closeOnce.Do(func() { close(q.closeCh) })
	return nil
}

func (q *Queue) enqueueAt(j jobs.Job, at time.Time) (string, error) {
	select {
	case <-q.closeCh:
		return "", fmt.Errorf("jobs/inproc: queue is closed")
	default:
	}
	if j.ID == "" {
		j.ID = newID()
	}
	e := &entry{
		job:       j,
		status:    jobs.StatusEnqueued,
		notBefore: at,
	}
	q.mu.Lock()
	q.entries = append(q.entries, e)
	q.byID[j.ID] = e
	q.mu.Unlock()

	// Pulse the notification channel (non-blocking).
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
	return j.ID, nil
}

// ── Source (consumer) ─────────────────────────────────────────────────────────

// Dequeue claims up to n jobs that are ready to run, honouring GroupKey
// serialisation. It returns immediately with whatever is ready; the worker
// handles the sleep loop.
func (q *Queue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	select {
	case <-q.closeCh:
		return nil, fmt.Errorf("jobs/inproc: queue is closed")
	default:
	}

	// Drain the notify channel so the next empty-result call doesn't spin.
	select {
	case <-q.notifyCh:
	default:
	}

	now := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()

	var picked []jobs.Job
	for _, e := range q.entries {
		if len(picked) >= n {
			break
		}
		if e.status != jobs.StatusEnqueued && e.status != jobs.StatusFailed {
			continue
		}
		if e.notBefore.After(now) {
			continue
		}
		// GroupKey serialisation.
		if e.job.GroupKey != "" && q.runningKeys[e.job.GroupKey] > 0 {
			continue
		}
		e.status = jobs.StatusRunning
		e.attempts++
		if e.job.GroupKey != "" {
			q.runningKeys[e.job.GroupKey]++
		}
		j := e.job
		j.Attempts = e.attempts
		picked = append(picked, j)
	}
	return picked, nil
}

func (q *Queue) Ack(ctx context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return nil
	}
	e.status = jobs.StatusSucceeded
	q.releaseGroup(e.job.GroupKey)
	q.removeEntry(id)
	return nil
}

func (q *Queue) Nack(ctx context.Context, id string, jobErr error, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return nil
	}
	q.releaseGroup(e.job.GroupKey)
	maxRetry := jobs.MaxRetryFor(e.job)
	if e.attempts >= maxRetry {
		q.removeEntry(id)
		return nil
	}
	e.status = jobs.StatusEnqueued
	e.notBefore = time.Now().Add(delay)
	// Pulse so the worker wakes when the delay elapses.
	go func() {
		select {
		case <-time.After(delay):
			select {
			case q.notifyCh <- struct{}{}:
			default:
			}
		case <-q.closeCh:
		}
	}()
	return nil
}

// Requeue returns j to the queue without spending a retry attempt: it restores
// the entry to enqueued with j's attempt count and headers (which the Worker
// uses to carry the unhandled-requeue counter). Unlike Nack it does not consult
// the retry budget — the Worker uses it for a job of a type this worker cannot
// handle, and an unhandled round-trip must not erode the budget a real handler
// will need (audit JB-4/JB-9). It also releases the GroupKey the delivery held.
func (q *Queue) Requeue(ctx context.Context, j jobs.Job, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[j.ID]
	if !ok {
		return nil
	}
	q.releaseGroup(e.job.GroupKey)
	e.job = j
	e.attempts = j.Attempts
	e.status = jobs.StatusEnqueued
	e.notBefore = time.Now().Add(delay)
	go func() {
		select {
		case <-time.After(delay):
			select {
			case q.notifyCh <- struct{}{}:
			default:
			}
		case <-q.closeCh:
		}
	}()
	return nil
}

func (q *Queue) Dead(ctx context.Context, id string, jobErr error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return nil
	}
	q.releaseGroup(e.job.GroupKey)
	q.removeEntry(id)
	return nil
}

// Cancel marks a job as cancelled. Implements jobs.Cancellable.
func (q *Queue) Cancel(ctx context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("jobs/inproc: job %s not found", id)
	}
	if e.status == jobs.StatusRunning {
		return fmt.Errorf("jobs/inproc: job %s is already running", id)
	}
	e.status = jobs.StatusCancelled
	// Cancelled is terminal, so drop the entry — as Ack, Dead and an exhausted
	// Nack all do. Cancel was the one terminal path that kept it, which leaked an
	// entry per cancellation for the life of the process and left it in the slice
	// Dequeue walks on every call, so a long-lived queue went on paying for
	// cancellations it had already served (audit JB-15).
	q.removeEntry(id)
	return nil
}

// Get implements jobs.Inspector.
func (q *Queue) Get(ctx context.Context, id string) (jobs.JobState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byID[id]
	if !ok {
		return jobs.JobState{}, fmt.Errorf("jobs/inproc: job %s not found", id)
	}
	return jobs.JobState{Job: e.job, Status: e.status}, nil
}

// List implements jobs.Inspector.
func (q *Queue) List(ctx context.Context, qry jobs.ListQuery) ([]jobs.JobState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	limit := qry.Limit
	if limit <= 0 {
		limit = 100
	}
	var out []jobs.JobState
	skipped := 0
	for _, e := range q.entries {
		if qry.Status != "" && e.status != qry.Status {
			continue
		}
		if qry.Type != "" && e.job.Type != qry.Type {
			continue
		}
		if qry.ActorID != "" && e.job.ActorID != qry.ActorID {
			continue
		}
		if qry.TenantID != "" && e.job.TenantID != qry.TenantID {
			continue
		}
		if skipped < qry.Offset {
			skipped++
			continue
		}
		out = append(out, jobs.JobState{Job: e.job, Status: e.status})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// releaseGroup drops one running-job count for key, forgetting the key entirely
// once nothing holds it.
//
// The delete is not tidiness: leaving a zero behind kept one map entry per
// distinct GroupKey the queue had ever seen, for the life of the process. Group
// keys are usually high-cardinality — a user, tenant or invoice id — so that grew
// without bound on exactly the workloads that use them. A missing key and a zero
// key read the same to Dequeue, which only asks whether the count is > 0, so
// nothing else changes (same class as audit JB-15, which the audit names only for
// Cancel).
func (q *Queue) releaseGroup(key string) {
	if key == "" {
		return
	}
	if q.runningKeys[key] > 1 {
		q.runningKeys[key]--
		return
	}
	delete(q.runningKeys, key)
}

func (q *Queue) removeEntry(id string) {
	for i, e := range q.entries {
		if e.job.ID == id {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			break
		}
	}
	delete(q.byID, id)
}

// newID returns a time-sortable random identifier (26-char base32, no padding),
// structurally similar to a ULID. Uses only stdlib — no third-party imports.
func newID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// Fill the random half — ignore the error; rand.Read never fails on real systems.
	tmp := make([]byte, 10)
	_, _ = rand.Read(tmp)
	binary.BigEndian.PutUint16(b[6:8], binary.BigEndian.Uint16(tmp[0:2]))
	copy(b[8:], tmp[2:])
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}

// Compile-time interface checks.
var (
	_ jobs.Queue       = (*Queue)(nil)
	_ jobs.Source      = (*Queue)(nil)
	_ jobs.Cancellable = (*Queue)(nil)
	_ jobs.Inspector   = (*Queue)(nil)
	_ jobs.Requeuer    = (*Queue)(nil)
)
