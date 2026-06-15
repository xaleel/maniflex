// Package cron provides a best-effort scheduled trigger for maniflex jobs.
//
// Each entry fires at a fixed interval and calls Queue.EnqueueAt to submit a
// job. If a replica is down when the interval fires, the schedule is missed
// (no durable cron). For durable scheduling, pair jobs/sql with a next_fire_at
// column in your own schema.
package cron

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"maniflex/jobs"
)

// Entry describes a recurring job schedule.
type Entry struct {
	// Every is the interval between firings.
	Every time.Duration
	// Job is the template submitted on each firing. ID is auto-assigned.
	Job jobs.Job
}

// Scheduler fires job templates on their configured intervals.
type Scheduler struct {
	queue   jobs.Queue
	entries []Entry
	logger  *slog.Logger

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a Scheduler that enqueues jobs via queue.
func New(queue jobs.Queue, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{queue: queue, logger: logger}
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
