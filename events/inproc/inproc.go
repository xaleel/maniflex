// Package inproc provides an in-process event bus for maniflex.
//
// Each subscription owns a bounded queue drained by Subscription.Concurrency
// workers. Publish enqueues and returns; it never blocks, and returns
// ErrQueueFull when a subscription's queue has no room — so a slow handler
// causes visible backpressure rather than an invisible pile-up, and a handler
// that publishes to its own bus cannot deadlock itself. Size the queue with
// Options.QueueSize.
//
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

// ErrQueueFull is returned by Publish when a matching subscription's queue has
// no room. It means the bus is falling behind, not that delivery failed: the
// event was never accepted, so the caller still holds it and can log, shed, or
// retry.
var ErrQueueFull = errors.New("inproc: subscription queue is full")

const (
	// defaultDrainTimeout bounds Close. Long enough for an ordinary handler to
	// finish, short enough that a wedged one cannot hold shutdown open forever.
	defaultDrainTimeout = 30 * time.Second

	// defaultQueueSize is the per-subscription backlog. Deep enough to absorb an
	// ordinary burst, shallow enough that a stalled handler is noticed while the
	// memory held is still trivial.
	defaultQueueSize = 1024
)

// Options customises the Bus. All fields are optional.
type Options struct {
	// DrainTimeout bounds how long Close waits for in-flight deliveries before
	// giving up and returning an error. Default: 30s.
	DrainTimeout time.Duration

	// QueueSize is the per-subscription backlog depth. Default: 1024.
	//
	// Publish returns ErrQueueFull once a subscription's queue is full rather
	// than waiting, so a slow handler cannot stall the caller and a handler that
	// publishes to its own bus cannot deadlock itself.
	QueueSize int
}

// Bus is an in-process fan-out event bus.
type Bus struct {
	mu sync.RWMutex
	// Pointers, not values: a subscription owns a sync.Once and a WaitGroup, and
	// Publish snapshots this slice — copying either type is a bug vet catches.
	subs []*subscription
	seq  atomic.Uint64

	drainTimeout time.Duration
	queueSize    int

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

// subscription is a bounded queue drained by a fixed pool of workers.
//
// Publish used to start one goroutine per subscriber per event, each blocking on
// a semaphore. That bounds how many handlers *run*, not how many goroutines wait
// behind them — so a handler slower than the publish rate accumulated goroutines
// without limit, each pinning its own copy of the event (audit EV-11). The queue
// bounds the backlog itself, and the pool replaces the semaphore as the cap on
// concurrent handlers.
type subscription struct {
	id     uint64
	sub    events.Subscription
	queue  chan events.Event
	stop   chan struct{}  // closed by Cancel to retire this subscription's workers
	once   sync.Once      // guards stop, so Cancel stays safe to call twice
	worker sync.WaitGroup // this subscription's workers
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
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Bus{
		drainTimeout: o.DrainTimeout,
		queueSize:    o.QueueSize,
		rootCtx:      ctx,
		cancel:       cancel,
	}
}

// Publish delivers e to every matching subscription's queue, where that
// subscription's worker pool picks it up. At most Concurrency handlers run at
// once per subscription.
//
// Publish returns ErrQueueFull if any matching subscription has no room. It
// never blocks: a slow handler must not stall the caller, and a handler that
// publishes to its own bus would otherwise deadlock against a full queue.
//
// The event is still enqueued to every other matching subscription — one
// saturated consumer does not deny the rest their copy — so a non-nil return
// means "at least one subscriber did not accept this", not "nothing was
// delivered".
func (b *Bus) Publish(_ context.Context, e events.Event) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	b.mu.RLock()
	subs := make([]*subscription, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	var full error
	for _, s := range subs {
		if !matchesAny(s.sub.Patterns, e.Type) {
			continue
		}
		// Counted before the send, so a Close racing this Publish either sees
		// the counter and waits, or has already set closed and refused above.
		b.inflight.Add(1)
		select {
		case s.queue <- e:
		default:
			b.inflight.Done()
			full = fmt.Errorf("%w (subscription %d, capacity %d)", ErrQueueFull, s.id, cap(s.queue))
		}
	}
	return full
}

// runWorker drains one subscription's queue until the subscription is cancelled
// or the bus is closed. Concurrency of these run per subscription, which is what
// caps concurrent handlers now that the per-event semaphore is gone.
func (b *Bus) runWorker(s *subscription) {
	defer s.worker.Done()
	for {
		select {
		case e := <-s.queue:
			// b.rootCtx, not context.Background(): Close cancels it, so a
			// handler mid-retry is asked to stop instead of being abandoned
			// (audit EV-6).
			events.DeliverWithRetry(b.rootCtx, b, s.sub, e)
			b.inflight.Done()
		case <-s.stop:
			// Cancelled. Release the events still queued so Close's drain
			// cannot wait forever on deliveries that will never run.
			b.releaseQueued(s)
			return
		case <-b.rootCtx.Done():
			b.releaseQueued(s)
			return
		}
	}
}

// releaseQueued discounts the events left in a retired subscription's queue.
// Each was counted at Publish, so without this the inflight WaitGroup never
// reaches zero and Close blocks until its timeout on work nobody will do.
func (b *Bus) releaseQueued(s *subscription) {
	for {
		select {
		case <-s.queue:
			b.inflight.Done()
		default:
			return
		}
	}
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

	s := &subscription{
		id:    b.seq.Add(1),
		sub:   sub,
		queue: make(chan events.Event, b.queueSize),
		stop:  make(chan struct{}),
	}

	// Workers start before the subscription is visible to Publish, so nothing
	// can land in a queue nobody is draining.
	s.worker.Add(sub.Concurrency)
	for range sub.Concurrency {
		go b.runWorker(s)
	}

	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		for i, cur := range b.subs {
			if cur.id == s.id {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		// Unregistered first, so no further events are queued, then the workers
		// are retired. sync.Once keeps a second Cancel from closing stop twice.
		s.once.Do(func() { close(s.stop) })
		s.worker.Wait()
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
