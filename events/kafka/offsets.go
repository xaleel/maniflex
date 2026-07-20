package kafka

import (
	"sync"

	kafkago "github.com/segmentio/kafka-go"
)

// offsetTracker decides which messages are safe to commit when handlers finish
// out of order.
//
// Kafka offset commits are **cumulative per partition**: committing offset 4
// declares everything up to and including 4 consumed. With Subscription
// Concurrency above 1, each message is handled in its own goroutine and they
// finish in whatever order the handlers happen to take, so a goroutine that
// committed its own offset directly could commit 4 while 3 was still running.
// A crash at that moment resumes after 4 and message 3 is never delivered
// again — silently, since nothing observes the gap (audit EV-4).
//
// The tracker holds each completed message until every earlier offset on its
// partition has also completed, then releases the contiguous run as a single
// commit at its highest offset. In-flight work is therefore never covered by a
// commit, so a crash replays it rather than skipping it — at-least-once, which
// is the guarantee the consumer group is supposed to provide.
//
// Partitions are tracked independently because their offset sequences are
// independent; a slow handler on one partition must not hold up commits on
// another.
type offsetTracker struct {
	mu    sync.Mutex
	parts map[partitionKey]*partitionOffsets
}

type partitionKey struct {
	topic     string
	partition int
}

// partitionOffsets is the per-partition bookkeeping: the offset the next commit
// must start from, and the completed messages waiting on a gap below them.
type partitionOffsets struct {
	// next is the lowest offset not yet committed. Set from the first message
	// seen on the partition, since a consumer group may resume anywhere.
	next    int64
	started bool
	// inflight counts messages fetched but not yet completed. When it is zero
	// there is no run to preserve, which is what makes resyncing safe.
	inflight int
	// pending holds completed messages whose offset is above next, keyed by
	// offset. They are released once the gap below them closes.
	pending map[int64]kafkago.Message
}

func newOffsetTracker() *offsetTracker {
	return &offsetTracker{parts: make(map[partitionKey]*partitionOffsets)}
}

// track records that a message has been fetched and will be handled. It must be
// called for every message before the handler runs, in fetch order, so the
// tracker knows where a partition's sequence begins.
func (t *offsetTracker) track(msg kafkago.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.partition(msg)
	switch {
	case !p.started:
		p.next = msg.Offset
		p.started = true
	case p.inflight == 0 && msg.Offset > p.next:
		// The partition resumed ahead of where this tracker left off — a
		// consumer-group rebalance can revoke a partition and hand it back
		// after another member has advanced it. With nothing in flight there
		// is no run left to protect, so resync rather than waiting forever on
		// offsets that will never be fetched: every later completion would
		// pile up in pending and the partition would stop committing entirely.
		p.next = msg.Offset
		clear(p.pending)
	}
	p.inflight++
}

// complete records that a message has been handled and returns the message to
// commit, if any. A nil return means an earlier offset on the same partition is
// still in flight and nothing may be committed yet.
//
// The returned message is the highest of the contiguous completed run, which
// commits that message and every one below it in a single call — exactly what
// cumulative commits mean.
func (t *offsetTracker) complete(msg kafkago.Message) (kafkago.Message, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	p := t.partition(msg)
	if !p.started {
		// complete without a preceding track: treat this message as the start
		// of the sequence rather than dropping it on the floor.
		p.next = msg.Offset
		p.started = true
	} else if p.inflight > 0 {
		p.inflight--
	}
	if msg.Offset < p.next {
		// Already covered by an earlier commit. Committing again would move the
		// group's offset backwards and replay everything above it.
		return kafkago.Message{}, false
	}
	p.pending[msg.Offset] = msg

	var commit kafkago.Message
	var found bool
	for {
		m, ok := p.pending[p.next]
		if !ok {
			break
		}
		delete(p.pending, p.next)
		commit, found = m, true
		p.next++
	}
	if !found {
		return kafkago.Message{}, false
	}
	return commit, true
}

// pendingCount reports how many completed messages are held back waiting on a
// gap. Used by tests to assert the tracker is not leaking memory once a
// partition drains.
func (t *offsetTracker) pendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, p := range t.parts {
		n += len(p.pending)
	}
	return n
}

// partition returns the bookkeeping for msg's partition, creating it on first
// use. Callers must hold t.mu.
func (t *offsetTracker) partition(msg kafkago.Message) *partitionOffsets {
	k := partitionKey{topic: msg.Topic, partition: msg.Partition}
	p, ok := t.parts[k]
	if !ok {
		p = &partitionOffsets{pending: make(map[int64]kafkago.Message)}
		t.parts[k] = p
	}
	return p
}
