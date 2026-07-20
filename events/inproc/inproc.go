// Package inproc provides an in-process event bus for maniflex.
// Events are delivered to subscribers via a bounded worker pool whose size is
// controlled by Subscription.Concurrency.
// There is no persistence: events published before a subscription is registered,
// or while the process is down, are lost.
//
// Use inproc for tests and single-binary deployments.
// For durability pair with events/outbox.
//
// # Shutdown
//
// Call Close before the process exits. It stops accepting events and waits for
// in-flight handlers, bounded by Options.DrainTimeout, then cancels the ones
// that overran. A non-nil error means the drain did not complete — deliveries
// were interrupted, so those events were not processed. Skipping Close leaves
// handlers to die with the process, which turns at-least-once into
// at-most-once at exactly the moment a deploy makes that most likely.
package inproc

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xaleel/maniflex/events"
)

// ErrBusClosed is returned by Publish after Close. A closed Bus starts no new
// deliveries, so accepting the event would silently drop it — the caller is told
// instead, and can distinguish shutdown from a delivery failure.
var ErrBusClosed = errors.New("inproc: bus is closed")

// defaultDrainTimeout bounds Close. Long enough for an ordinary handler to
// finish, short enough that a wedged one cannot hold shutdown open forever.
const defaultDrainTimeout = 30 * time.Second

// Options customises the Bus. All fields are optional.
type Options struct {
	// DrainTimeout bounds how long Close waits for in-flight deliveries before
	// giving up and returning an error. Default: 30s.
	DrainTimeout time.Duration
}

// Bus is an in-process fan-out event bus.
type Bus struct {
	mu   sync.RWMutex
	subs []subscription
	seq  atomic.Uint64

	drainTimeout time.Duration

	// rootCtx is cancelled by Close, so in-flight handlers are asked to stop
	// rather than merely being outlived. Deliveries previously ran on
	// context.Background(), which is never cancelled — so DeliverWithRetry's
	// cancellation path could not fire and a handler mid-retry died with the
	// process (audit EV-6).
	rootCtx context.Context
	cancel  context.CancelFunc

	// inflight tracks delivery goroutines so Close can wait for them.
	inflight sync.WaitGroup
	closed   atomic.Bool
}

type subscription struct {
	id  uint64
	sub events.Subscription
	sem chan struct{} // bounded semaphore: capacity == sub.Concurrency
}

// New creates an in-process Bus.
func New(opts ...Options) *Bus {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = defaultDrainTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Bus{drainTimeout: o.DrainTimeout, rootCtx: ctx, cancel: cancel}
}

// Publish delivers e to all matching subscribers. Each delivery acquires a slot
// from the subscription's semaphore before calling DeliverWithRetry, so at most
// Concurrency handlers run concurrently per subscription.
func (b *Bus) Publish(_ context.Context, e events.Event) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	b.mu.RLock()
	subs := make([]subscription, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	for _, s := range subs {
		if !matchesAny(s.sub.Patterns, e.Type) {
			continue
		}
		// Add before the goroutine starts, so a Close racing this Publish
		// either sees the counter and waits, or has already set closed and
		// been refused above. Incrementing inside the goroutine would leave a
		// window where Close observes zero in-flight work and returns.
		b.inflight.Add(1)
		go func() {
			defer b.inflight.Done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			// b.rootCtx, not context.Background(): Close cancels it, so a
			// handler mid-retry is asked to stop instead of being abandoned.
			events.DeliverWithRetry(b.rootCtx, b, s.sub, e)
		}()
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

// Close stops accepting new events and waits for in-flight deliveries to
// finish, bounded by Options.DrainTimeout.
//
// It was previously a no-op returning nil, so a handler still running when the
// process exited was simply lost — at-least-once degrading to at-most-once at
// exactly the moment a deploy or scale-down makes that most likely (audit EV-6).
//
// Close first refuses new Publish calls, then waits. If the budget elapses it
// cancels the delivery context so handlers observing it can stop, waits a short
// grace period, and returns an error naming how many were still running. An
// error therefore means "shutdown was not clean", which is the thing a caller
// needs to be able to log or alert on.
//
// Individual subscriptions are still removed with their Cancel func; Close ends
// the whole bus. It is safe to call more than once.
func (b *Bus) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}

	done := make(chan struct{})
	go func() {
		b.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		b.cancel() // nothing left to signal; released so the context is not leaked
		return nil
	case <-time.After(b.drainTimeout):
	}

	// Budget spent. Cancel the stragglers and give them a moment to unwind
	// before returning, so the process is not torn down mid-unwind.
	//
	// Reaching here is always an error, even when they all stop promptly: work
	// was interrupted rather than completed, and a handler that returns early
	// because its context was cancelled has not delivered its event. Treating a
	// tidy unwind as a clean drain would report success for exactly the event
	// loss this fix exists to surface.
	b.cancel()
	select {
	case <-done:
	case <-time.After(drainGrace):
	}
	return fmt.Errorf("%w: deliveries were still running after %s and were cancelled",
		ErrDrainIncomplete, b.drainTimeout)
}

// drainGrace is how long Close waits after cancelling for handlers to notice and
// unwind. Short: it is a courtesy for well-behaved handlers, not a second budget.
const drainGrace = 250 * time.Millisecond

// ErrDrainIncomplete is returned by Close when deliveries were still running
// after the drain budget and the cancellation grace period.
var ErrDrainIncomplete = errors.New("inproc: drain incomplete")

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
