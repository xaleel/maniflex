// Package cron provides a best-effort scheduled trigger for maniflex jobs.
//
// Each entry fires at a fixed interval and calls Queue.EnqueueAt to submit a
// job. If a replica is down when the interval fires, the schedule is missed
// (no durable cron). For durable scheduling, pair jobs/sql with a next_fire_at
// column in your own schema.
//
// # Running on more than one replica
//
// A Scheduler ticks in its own process and knows nothing about its peers, so
// three replicas each running one enqueue the job three times per interval.
// Run the Scheduler in exactly one process, or pass WithLocker so the replicas
// elect a single winner per interval (audit JB-16).
package cron

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// Entry describes a recurring job schedule.
type Entry struct {
	// Every is the interval between firings.
	Every time.Duration
	// Job is the template submitted on each firing. ID is auto-assigned.
	Job jobs.Job
	// Name distinguishes this entry's lock key when a Locker is configured.
	// Optional; defaults to Job.Type. Every is already part of the key, so this
	// only matters for two entries that share both a Type and an interval —
	// the same job template fired with different payloads, say. It must be
	// identical across replicas: it is how they recognise the same schedule.
	Name string
}

// Locker elects one replica per firing, so a schedule running on N replicas
// enqueues its job once per interval instead of N times.
//
// Acquire must be atomic across replicas and report true to exactly one caller
// per key: Redis SET key val NX PX ttl, an INSERT on a unique column, a
// Postgres advisory lock — whatever the deployment already runs.
//
// Unlike middleware/idempotency.Locker there is no release func, and that is
// deliberate. The lock records "this interval has been claimed", not "this work
// is in progress", so it must outlive the firing: releasing it would let the
// next replica to tick within the same interval fire again. Let it expire with
// ttl instead. Keys are per-interval and never revisited, so an implementation
// only needs the expiry to reclaim storage.
type Locker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithLocker gates every firing behind locker so that only one replica enqueues
// each interval. Without it — the default — every Scheduler fires independently.
func WithLocker(l Locker) Option {
	return func(s *Scheduler) { s.locker = l }
}

// Scheduler fires job templates on their configured intervals.
type Scheduler struct {
	queue   jobs.Queue
	entries []Entry
	logger  *slog.Logger
	locker  Locker

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a Scheduler that enqueues jobs via queue.
func New(queue jobs.Queue, logger *slog.Logger, opts ...Option) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scheduler{queue: queue, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Add registers a recurring entry. Must be called before Start.
func (s *Scheduler) Add(e Entry) {
	s.entries = append(s.entries, e)
}

// Start launches one ticker goroutine per entry. It returns immediately;
// use Stop to halt all tickers.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	tickCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	for _, e := range s.entries {
		e := e // capture
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(tickCtx, e)
		}()
	}
}

// Stop cancels all ticker goroutines and waits for them to exit.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context, e Entry) {
	ticker := time.NewTicker(e.Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if !s.claim(ctx, e, t) {
				continue
			}
			j := e.Job
			j.ID = "" // let the queue assign a fresh ID each firing
			if _, err := s.queue.EnqueueAt(ctx, j, t); err != nil {
				s.logger.Error("[cron] enqueue failed",
					slog.String("type", e.Job.Type),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// claim reports whether this replica may fire e for the interval containing t.
//
// With no Locker every replica fires, which is the historical behaviour and the
// documented single-process contract.
func (s *Scheduler) claim(ctx context.Context, e Entry, t time.Time) bool {
	if s.locker == nil {
		return true
	}
	key := fireKey(e, t)
	ok, err := s.locker.Acquire(ctx, key, e.Every)
	if err != nil {
		// Fail open, as middleware/idempotency does on a Locker error. A lock
		// backend outage must not silently stop the schedule: duplicate firings
		// while it is down are visible and recoverable, whereas a nightly job
		// that quietly never ran is neither.
		s.logger.Warn("[cron] lock unavailable, firing anyway",
			slog.String("type", e.Job.Type),
			slog.String("key", key),
			slog.String("error", err.Error()),
		)
		return true
	}
	if !ok {
		s.logger.Debug("[cron] interval claimed by another replica",
			slog.String("type", e.Job.Type),
			slog.String("key", key),
		)
	}
	return ok
}

// fireKey identifies one interval of one entry, in the form
// "cron|<name>|<every>|<bucket-unix>".
//
// The fire time is truncated to Every so that replicas started at different
// moments still agree on the interval: time.Truncate rounds down against the
// zero time, not against process start, so a 24h entry buckets on UTC midnight
// however long each replica has been up. Without that, one replica ticking at
// :03 and another at :47 would never contend for the same key and the Locker
// would be decorative.
func fireKey(e Entry, t time.Time) string {
	name := e.Name
	if name == "" {
		name = e.Job.Type
	}
	return fmt.Sprintf("cron|%s|%s|%d", name, e.Every, t.Truncate(e.Every).Unix())
}
