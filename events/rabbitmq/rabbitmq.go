// Package rabbitmq provides an AMQP 0.9.1 (RabbitMQ) event bus adapter for maniflex events.
//
// # Architecture
//
// RabbitMQ is queue-based, not log-based. Once a message is acknowledged, it is gone;
// there is no offset-rewind or replay. This is a fundamental difference from the
// streaming brokers (Redis, NATS, Kafka).
//
// Mapping:
//   - Publish uses a topic exchange. The routing key is set to event.Type
//     (e.g. "invoice.created"), matching the pattern directly.
//   - Subscribe.Group becomes a named durable queue with competing consumers.
//   - Pattern matching ("invoice.*") becomes a topic-exchange binding with the
//     routing key pattern as the binding key.
//
// # Replay warning
//
// RabbitMQ does NOT provide replay. Teams that need replay should pair this adapter
// with events/outbox (which keeps the durable log in the application DB) or choose
// a streaming broker (Redis Streams, NATS JetStream, Kafka).
//
// # Reconnection
//
// amqp091-go connections do not self-heal, so recovering from a drop means
// dialing a new one — which the bus can only do if it owns the connection.
// Which constructor you use decides that:
//
//   - NewWithDialer redials and rebuilds. A dropped subscription re-declares its
//     exchange, queue, bindings and prefetch on a fresh channel and resumes,
//     paced by Options.ReconnectBackoff. Prefer this for anything long-running.
//   - New takes a *amqp.Connection you own and cannot replace it. A drop ends
//     every subscription on it for the life of the process — the app keeps
//     serving while the queue silently stops being consumed. The death is made
//     loud instead: an ERROR naming the queue, and Options.OnSubscriptionClosed
//     so you can alert or rebuild the bus yourself.
//
// Unacknowledged messages are requeued by the broker when the channel closes,
// so a rebuild resumes from them rather than losing them.
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xaleel/maniflex/events"
)

const exchangeName = "maniflex.events"

// defaultConfirmTimeout bounds the wait for a broker confirm, so a wedged or
// unreachable broker fails the publish instead of blocking the caller forever.
const defaultConfirmTimeout = 5 * time.Second

// Options customises the Bus. All fields are optional.
type Options struct {
	// OnSubscriptionClosed is called when a subscription stops for a reason
	// other than its Cancel being invoked — a connection or channel drop, or
	// the queue being deleted from under it.
	//
	// It fires only on a bus built by New, which cannot redial a connection it
	// does not own: without this callback a drop leaves the subscription dead
	// while the process keeps running and looking healthy. Use it to alert, or
	// to tear down and re-Subscribe on a fresh connection.
	//
	// A bus built by NewWithDialer rebuilds the subscription instead, so this is
	// never called there — the reconnect attempts are logged.
	//
	// It is called once per subscription, from the subscription's own goroutine.
	OnSubscriptionClosed func(queue string, err error)

	// ConfirmTimeout bounds how long Publish waits for the broker to confirm a
	// message. Default: 5s.
	ConfirmTimeout time.Duration

	// Prefetch is how many unacknowledged messages the broker may have
	// outstanding on a subscription's channel. Default: the subscription's
	// Concurrency, so a worker is never holding more than one message.
	//
	// Raise it to keep workers fed when handlers are fast and the round trip to
	// the broker is not; the cost is that many more messages in flight, which
	// are redelivered if the consumer dies. There is deliberately no
	// "unlimited": without a Qos the broker pushes the whole queue at one
	// consumer, which is memory the process never agreed to spend.
	Prefetch int

	// ReconnectBackoff paces the redial attempts after a subscription's channel
	// drops. Applies only to a bus built by NewWithDialer; the zero value uses
	// the shared defaults in events.ReadBackoff.
	ReconnectBackoff events.ReadBackoff
}

// Bus is an AMQP 0.9.1 event bus.
type Bus struct {
	// connMu guards conn on its own, not under mu: reopenPublishChannel holds
	// mu while opening a channel, which has to read conn, so one mutex for both
	// would deadlock the reconnect path.
	connMu sync.RWMutex
	conn   *amqp.Connection

	opts  Options
	mu    sync.Mutex
	pubCh *amqp.Channel // shared publish channel (lazy-opened, re-opened on error)

	// newConsumer opens the channel a subscription consumes on. Nil means the
	// real AMQP channel; tests substitute a fake, which is the only way to
	// reach the delivery, ack, and shutdown paths without a broker.
	newConsumer func() (consumerChannel, error)

	// dial redials the broker. Non-nil exactly when this bus owns its
	// connection, which is what licenses it to rebuild a dropped subscription:
	// a connection handed to New belongs to the caller and cannot be replaced
	// from here.
	dial func() (*amqp.Connection, error)
}

// consumerChannel is the set of AMQP operations one subscription performs on
// its channel. It exists so the delivery loop can be driven by a fake, the way
// events/redis drives its consumer through streamOps.
type consumerChannel interface {
	ExchangeDeclare() error
	QueueDeclare(queue string) error
	QueueBind(queue, bindingKey string) error
	Qos(prefetch int) error
	Consume(queue string) (<-chan amqp.Delivery, error)
	NotifyClose() <-chan *amqp.Error
	Close() error
}

// amqpConsumerChannel is the production consumerChannel, over a real channel.
type amqpConsumerChannel struct{ ch *amqp.Channel }

func (a amqpConsumerChannel) ExchangeDeclare() error { return declareExchange(a.ch) }

func (a amqpConsumerChannel) QueueDeclare(queue string) error {
	_, err := a.ch.QueueDeclare(queue, true, false, false, false, nil)
	return err
}

func (a amqpConsumerChannel) QueueBind(queue, bindingKey string) error {
	return a.ch.QueueBind(queue, bindingKey, exchangeName, false, nil)
}

// Qos applies per consumer rather than per channel (global=false): the bound
// belongs to this subscription, not to whatever else shares the connection.
func (a amqpConsumerChannel) Qos(prefetch int) error {
	return a.ch.Qos(prefetch, 0, false)
}

func (a amqpConsumerChannel) Consume(queue string) (<-chan amqp.Delivery, error) {
	return a.ch.Consume(queue, "", false, false, false, false, nil)
}

func (a amqpConsumerChannel) NotifyClose() <-chan *amqp.Error {
	return a.ch.NotifyClose(make(chan *amqp.Error, 1))
}

func (a amqpConsumerChannel) Close() error { return a.ch.Close() }

// openConsumer returns the channel a subscription will consume on.
func (b *Bus) openConsumer() (consumerChannel, error) {
	if b.newConsumer != nil {
		return b.newConsumer()
	}
	ch, err := b.openChannel()
	if err != nil {
		return nil, err
	}
	return amqpConsumerChannel{ch: ch}, nil
}

// New creates a RabbitMQ Bus from an existing AMQP connection.
// The topic exchange "maniflex.events" is declared as durable on first use.
//
// The publish channel runs in confirm mode, so Publish reports whether the
// broker actually took responsibility for the message rather than only whether
// it was written to a socket.
func New(conn *amqp.Connection, opts ...Options) (*Bus, error) {
	b := &Bus{conn: conn}
	if len(opts) > 0 {
		b.opts = opts[0]
	}
	if b.opts.ConfirmTimeout <= 0 {
		b.opts.ConfirmTimeout = defaultConfirmTimeout
	}
	ch, err := b.openPublishChannel()
	if err != nil {
		return nil, err
	}
	if err := declareExchange(ch); err != nil {
		ch.Close()
		return nil, err
	}
	b.pubCh = ch
	return b, nil
}

// NewWithDialer creates a Bus that owns its connection, and therefore rebuilds
// a subscription whose channel drops instead of reporting it dead.
//
// dial is called once now and again on every reconnect, so it must return a
// fresh connection each time — typically a closure over amqp.Dial and your URL.
// Close closes the connection the bus is currently holding.
//
//	bus, err := rabbitmq.NewWithDialer(func() (*amqp.Connection, error) {
//	    return amqp.Dial(os.Getenv("AMQP_URL"))
//	})
//
// Prefer this over New for anything long-running. amqp091-go connections do not
// self-heal, so a bus built by New stops consuming for the life of the process
// the first time its connection drops.
func NewWithDialer(dial func() (*amqp.Connection, error), opts ...Options) (*Bus, error) {
	if dial == nil {
		return nil, fmt.Errorf("rabbitmq: NewWithDialer needs a dialer")
	}
	conn, err := dial()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: dial: %w", err)
	}
	b, err := New(conn, opts...)
	if err != nil {
		conn.Close()
		return nil, err
	}
	b.dial = dial
	return b, nil
}

// redial replaces the connection this bus holds. Only a bus built by
// NewWithDialer has one to replace.
func (b *Bus) redial() error {
	if b.dial == nil {
		return fmt.Errorf("rabbitmq: this bus does not own its connection")
	}
	conn, err := b.dial()
	if err != nil {
		return fmt.Errorf("rabbitmq: dial: %w", err)
	}

	b.connMu.Lock()
	old := b.conn
	b.conn = conn
	b.connMu.Unlock()

	// The publish channel belonged to the old connection and died with it, so
	// a reconnect that only restored consumers would leave Publish failing for
	// ever against a channel that can never recover.
	b.reopenPublishChannel()

	if old != nil {
		old.Close()
	}
	return nil
}

// currentConn returns the connection the bus is holding, which redial replaces.
func (b *Bus) currentConn() *amqp.Connection {
	b.connMu.RLock()
	defer b.connMu.RUnlock()
	return b.conn
}

// Publish routes e to the topic exchange with routing key = e.Type.
func (b *Bus) Publish(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("rabbitmq: marshal: %w", err)
	}

	b.mu.Lock()
	ch := b.pubCh
	b.mu.Unlock()

	conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchangeName, e.Type, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    e.ID,
			Timestamp:    e.Time,
			Body:         payload,
		})
	if err != nil {
		b.reopenPublishChannel()
		return fmt.Errorf("rabbitmq: publish: %w", err)
	}

	// Writing to the socket is not delivery. Without waiting for the confirm,
	// Publish returned nil for a message the broker never persisted — an
	// unroutable event, a full disk, a node failing mid-write all looked like
	// success, and the event was gone with nothing to retry from (audit EV-5).
	wctx, cancel := context.WithTimeout(ctx, b.opts.ConfirmTimeout)
	defer cancel()
	acked, err := conf.WaitContext(wctx)
	switch {
	case err != nil:
		// Timed out or the caller's context ended: the broker's answer is
		// unknown, so the message may or may not be stored. Reported as a
		// failure because the safe reading of "unknown" is "not delivered" —
		// a redelivery is a duplicate, which dedupe handles, while assuming
		// success loses the event outright.
		b.reopenPublishChannel()
		return fmt.Errorf("rabbitmq: publish confirm for %s: %w", e.ID, err)
	case !acked:
		// The broker explicitly refused responsibility for the message.
		return fmt.Errorf("rabbitmq: broker nacked event %s (type %s)", e.ID, e.Type)
	}
	return nil
}

// reopenPublishChannel replaces the shared publish channel after an error on it.
// A channel is closed by the broker on any protocol error, so the next publish
// on it would fail too.
func (b *Bus) reopenPublishChannel() {
	b.mu.Lock()
	defer b.mu.Unlock()
	newCh, err := b.openPublishChannel()
	if err != nil {
		// Leave the old channel in place: the next publish fails against it and
		// tries again from here. Replacing it with nil would panic instead.
		slog.Default().Error("rabbitmq: could not reopen publish channel",
			slog.String("error", err.Error()))
		return
	}
	if b.pubCh != nil {
		b.pubCh.Close()
	}
	b.pubCh = newCh
}

// PublishBatch publishes all events in es sequentially on the shared channel.
func (b *Bus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe declares a durable queue bound to the exchange with each pattern
// as the binding key, then starts competing consumers for sub.Group.
//
// Pattern "invoice.*" binds to routing keys matching that AMQP topic pattern.
// Pattern "*" binds to "#" (all routing keys).
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

	// A durable queue per consumer group.
	queue := fmt.Sprintf("maniflex.%s", sub.Group)

	// The first attempt is the caller's to see: a queue that cannot be declared
	// or bound is a configuration error, and reporting it as a failed Subscribe
	// is more useful than retrying it forever in the background.
	ch, deliveries, err := b.openSubscription(sub, queue)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.consume(cctx, sub, queue, ch, deliveries)
	}()

	// Cancel waits for the loop, not merely for the handlers: with reconnection
	// the loop can be between attempts, and returning early would leave it to
	// open a channel after the caller believed the subscription was gone.
	return func() {
		cancel()
		<-done
	}, nil
}

// openSubscription opens a channel and gets it consuming: exchange, queue,
// bindings, prefetch bound, Consume. Every reconnect repeats the whole
// sequence, because a fresh channel knows none of it.
func (b *Bus) openSubscription(sub events.Subscription, queue string) (consumerChannel, <-chan amqp.Delivery, error) {
	ch, err := b.openConsumer()
	if err != nil {
		return nil, nil, err
	}
	if err := ch.ExchangeDeclare(); err != nil {
		ch.Close()
		return nil, nil, err
	}
	if err := ch.QueueDeclare(queue); err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("rabbitmq: declare queue: %w", err)
	}
	for _, pattern := range sub.Patterns {
		bindKey := patternToBindingKey(pattern)
		if err := ch.QueueBind(queue, bindKey); err != nil {
			ch.Close()
			return nil, nil, fmt.Errorf("rabbitmq: bind %q: %w", bindKey, err)
		}
	}
	// Bound what the broker may push at this consumer. Without it prefetch is
	// unlimited: the broker sends the whole queue, the process holds every
	// message it has not yet acked, and a backlog it never asked for becomes
	// its memory problem (blocker B2).
	if err := ch.Qos(b.prefetchFor(sub)); err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("rabbitmq: set prefetch: %w", err)
	}
	deliveries, err := ch.Consume(queue)
	if err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("rabbitmq: consume: %w", err)
	}
	return ch, deliveries, nil
}

// consume runs the delivery loop, rebuilding the subscription when the broker
// drops it and this bus owns its connection. A bus built by New does not, so
// there it reports the death and stops — the old behaviour, kept because a
// connection the caller owns cannot be replaced from here.
func (b *Bus) consume(cctx context.Context, sub events.Subscription, queue string, ch consumerChannel, deliveries <-chan amqp.Delivery) {
	backoff := b.opts.ReconnectBackoff
	for {
		// The broker's reason for closing this channel. Read only after
		// deliveries closes, by which point amqp091-go has already delivered
		// the error (or closed the channel, giving nil for a clean shutdown).
		closeErr := ch.NotifyClose()
		dropped := b.session(cctx, sub, ch, deliveries)
		ch.Close()

		switch afterSession(dropped, cctx.Err(), b.dial != nil) {
		case stopQuietly:
			return
		case reportAndStop:
			b.reportSubscriptionClosed(cctx, queue, closeErr)
			return
		}

		next, nextDeliveries, ok := b.reopenSubscription(cctx, sub, queue, &backoff)
		if !ok {
			return
		}
		ch, deliveries = next, nextDeliveries
	}
}

// sessionOutcome is what to do once a consume session has ended.
type sessionOutcome int

const (
	// stopQuietly: the caller asked for this. Cancel closes the delivery
	// channel too, so a shutdown can surface as a drop — and treating that as
	// an outage would both alert falsely and open a channel after the caller
	// believed the subscription was gone.
	stopQuietly sessionOutcome = iota
	// reportAndStop: the broker dropped us and this bus cannot redial, because
	// its connection belongs to whoever passed it to New.
	reportAndStop
	// rebuild: the broker dropped us and this bus owns its connection.
	rebuild
)

// afterSession decides what a finished session means. It is a function rather
// than three conditions inline because the cancellation arm is unreachable to
// test through the loop: Cancel and a channel drop can be ready at the same
// instant, and which one a select picks is not the caller's to arrange.
func afterSession(dropped bool, ctxErr error, canRedial bool) sessionOutcome {
	if !dropped || ctxErr != nil {
		return stopQuietly
	}
	if !canRedial {
		return reportAndStop
	}
	return rebuild
}

// reopenSubscription redials and rebuilds until it succeeds or the context
// ends. It never gives up on its own: a broker that is down is expected back,
// and a consumer that stopped trying is the outage this blocker was about.
func (b *Bus) reopenSubscription(cctx context.Context, sub events.Subscription, queue string, backoff *events.ReadBackoff) (consumerChannel, <-chan amqp.Delivery, bool) {
	for {
		attempt, delay, escalate := backoff.Next()
		if !backoff.Wait(cctx, delay) {
			return nil, nil, false
		}
		if err := b.redial(); err != nil {
			logReconnectFailure(queue, attempt, escalate, err)
			continue
		}
		ch, deliveries, err := b.openSubscription(sub, queue)
		if err != nil {
			logReconnectFailure(queue, attempt, escalate, err)
			continue
		}
		slog.Default().Info("rabbitmq: subscription resumed",
			slog.String("queue", queue),
			slog.Int("attempt", attempt))
		backoff.Reset()
		return ch, deliveries, true
	}
}

// logReconnectFailure reports a failed rebuild. It escalates once, when the
// backoff first reaches its ceiling — the point at which a run of failures
// stopped looking like a blip — and stays at WARN after, so a long outage does
// not bury the logs in ERRORs. Same shape as the kafka and redis read loops.
func logReconnectFailure(queue string, attempt int, escalate bool, err error) {
	attrs := []any{
		slog.String("queue", queue),
		slog.Int("attempt", attempt),
		slog.String("error", err.Error()),
	}
	if escalate {
		slog.Default().Error("rabbitmq: subscription still down after repeated reconnects",
			append(attrs, slog.String("impact", "events routed to this queue are not being consumed"))...)
		return
	}
	slog.Default().Warn("rabbitmq: reconnect failed, retrying", attrs...)
}

// session consumes until the delivery channel closes or the context ends. It
// reports true when the channel closed under it — the drop a reconnect answers.
func (b *Bus) session(cctx context.Context, sub events.Subscription, ch consumerChannel, deliveries <-chan amqp.Delivery) (dropped bool) {
	sem := make(chan struct{}, sub.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-cctx.Done():
			return false
		case msg, ok := <-deliveries:
			if !ok {
				// Any connection or channel drop lands here. Returning quietly,
				// as this used to, made a total consumer outage
				// indistinguishable from a healthy idle service (audit EV-5).
				return true
			}
			var e events.Event
			if err := json.Unmarshal(msg.Body, &e); err != nil {
				msg.Nack(false, false)
				continue
			}
			if !matchesAny(sub.Patterns, e.Type) {
				msg.Ack(false)
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() {
					<-sem
					wg.Done()
				}()
				if shouldAck(events.DeliverWithRetry(cctx, b, sub, e), cctx.Err()) {
					msg.Ack(false)
				}
			}()
		}
	}
}

// reportSubscriptionClosed announces that a subscription has stopped consuming.
//
// A cancelled context means the caller asked for this, so it is not reported:
// Cancel closes the delivery channel too, and treating an orderly shutdown as an
// outage would train operators to ignore the alert that matters.
func (b *Bus) reportSubscriptionClosed(ctx context.Context, queue string, closeErr <-chan *amqp.Error) {
	if ctx.Err() != nil {
		return
	}

	var err error
	select {
	case amqpErr := <-closeErr:
		if amqpErr != nil {
			err = amqpErr
		}
	default:
	}
	if err == nil {
		// The delivery channel closed without the broker giving a reason —
		// a basic.cancel (the queue was deleted) reaches us this way.
		err = fmt.Errorf("delivery channel closed without an error; the queue may have been deleted")
	}

	slog.Default().Error("rabbitmq: subscription stopped and will not reconnect",
		slog.String("queue", queue),
		slog.String("error", err.Error()),
		slog.String("impact", "events routed to this queue are no longer consumed"))

	if b.opts.OnSubscriptionClosed != nil {
		b.opts.OnSubscriptionClosed(queue, err)
	}
}

// Close closes the underlying AMQP connection — the one the bus is currently
// holding, which for a NewWithDialer bus may not be the one it started with.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.pubCh != nil {
		b.pubCh.Close()
		b.pubCh = nil
	}
	b.mu.Unlock()

	conn := b.currentConn()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (b *Bus) openChannel() (*amqp.Channel, error) {
	conn := b.currentConn()
	if conn == nil {
		return nil, fmt.Errorf("rabbitmq: no connection")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	return ch, nil
}

// openPublishChannel opens a channel in confirm mode. Confirm mode is per
// channel and cannot be turned on later, so every replacement publish channel
// must go through here — a plain openChannel would silently downgrade Publish
// to fire-and-forget after the first reconnect.
func (b *Bus) openPublishChannel() (*amqp.Channel, error) {
	ch, err := b.openChannel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		ch.Close()
		return nil, fmt.Errorf("rabbitmq: enable publisher confirms: %w", err)
	}
	return ch, nil
}

func declareExchange(ch *amqp.Channel) error {
	return ch.ExchangeDeclare(exchangeName, "topic", true, false, false, false, nil)
}

// patternToBindingKey converts a glob pattern to an AMQP topic binding key.
// "*" → "#" (all), "invoice.*" → "invoice.*", "invoice.created" → "invoice.created".
// prefetchFor returns the unacknowledged-message bound for one subscription:
// Options.Prefetch when set, otherwise the subscription's Concurrency, so a
// worker holds at most one message and the bound rises with the workers that
// have to drain it.
func (b *Bus) prefetchFor(sub events.Subscription) int {
	if b.opts.Prefetch > 0 {
		return b.opts.Prefetch
	}
	return sub.Concurrency
}

// shouldAck decides whether a delivered message is acknowledged.
//
// As Kafka: an unsettled delivery on a consumer that keeps running is acked
// anyway, and the event whose dead-lettering also failed is lost. Redis and
// NATS withhold here and let the broker redeliver.
//
// The reason used to be that prefetch was unlimited, so withheld messages
// accumulated without bound. Prefetch is now bounded (blocker B2), and the
// answer did not change — it got worse. Withheld messages are redelivered only
// when the channel closes, so with prefetch N a run of N unsettled deliveries
// fills the window and the consumer receives nothing further, for the life of
// the process. A handful of poison events would end consumption entirely, which
// is a heavier failure than losing the events themselves. Per-message
// acknowledgement is what lets redis and nats withhold one message without
// blocking the next; AMQP's prefetch window does not.
//
// This is a published delivery guarantee — see the adapter matrix in
// docs/src/advanced-topics/events-jobs.md — so a change here is a behaviour
// change rather than an implementation detail.
func shouldAck(settled bool, ctxErr error) bool {
	return settled || ctxErr == nil
}

func patternToBindingKey(pattern string) string {
	if pattern == "*" {
		return "#"
	}
	return pattern
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
