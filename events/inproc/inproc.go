// Package inproc provides an in-process event bus for maniflex.
// Events are delivered to subscribers via a bounded worker pool whose size is
// controlled by Subscription.Concurrency.
// There is no persistence: events published before a subscription is registered,
// or while the process is down, are lost.
//
// Use inproc for tests and single-binary deployments.
// For durability pair with events/outbox.
package inproc

import (
	"context"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"maniflex/events"
)

// Bus is an in-process fan-out event bus.
type Bus struct {
	mu   sync.RWMutex
	subs []subscription
	seq  atomic.Uint64
}

type subscription struct {
	id  uint64
	sub events.Subscription
	sem chan struct{} // bounded semaphore: capacity == sub.Concurrency
}

// New creates an in-process Bus.
func New() *Bus { return &Bus{} }

// Publish delivers e to all matching subscribers. Each delivery acquires a slot
// from the subscription's semaphore before calling DeliverWithRetry, so at most
// Concurrency handlers run concurrently per subscription.
func (b *Bus) Publish(_ context.Context, e events.Event) error {
	b.mu.RLock()
	subs := make([]subscription, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	for _, s := range subs {
		if matchesAny(s.sub.Patterns, e.Type) {
			s := s
			go func() {
				s.sem <- struct{}{}
				defer func() { <-s.sem }()
				events.DeliverWithRetry(context.Background(), b, s.sub, e)
			}()
		}
	}
	return nil
}

// PublishBatch publishes each event in es. Each delivery is independent.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe registers sub and returns a Cancel function to remove it.
// The returned Cancel is safe to call more than once.
func (b *Bus) Subscribe(_ context.Context, sub events.Subscription) (events.Cancel, error) {
	if sub.Concurrency <= 0 {
		sub.Concurrency = 1
	}
	if sub.MaxRetry <= 0 {
		sub.MaxRetry = 3
	}
	if sub.Backoff == nil {
		sub.Backoff = linearBackoff
	}
	if len(sub.Patterns) == 0 {
		sub.Patterns = []string{"*"}
	}

	id := b.seq.Add(1)

	b.mu.Lock()
	b.subs = append(b.subs, subscription{
		id:  id,
		sub: sub,
		sem: make(chan struct{}, sub.Concurrency),
	})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			for i, s := range b.subs {
				if s.id == id {
					b.subs = append(b.subs[:i], b.subs[i+1:]...)
					break
				}
			}
			b.mu.Unlock()
		})
	}, nil
}

// Close is a no-op; in-process subscriptions are cancelled via their Cancel func.
func (b *Bus) Close() error { return nil }

func linearBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

// matchesAny reports whether eventType matches any glob pattern.
// Uses path.Match syntax: "invoice.*", "*.created", "*".
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
