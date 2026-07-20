package kafka

// Audit EV-4: Kafka offset commits are cumulative per partition, so committing
// offset 4 declares 0..4 consumed. With Concurrency > 1 each message was handled
// in its own goroutine and committed its own offset directly, so a fast handler
// on offset 4 could commit while offset 3 was still running — a crash then
// resumed after 4 and message 3 was never redelivered.
//
// There is no Kafka broker in this environment, so the ordering rule is tested
// as a unit. That is where the defect lives: the surrounding fetch loop is a
// thin wrapper, but "which offset is safe to commit" is the whole bug.
//
//	go test ./events/kafka/... -run TestOffsetTracker

import (
	"sync"
	"testing"

	kafkago "github.com/segmentio/kafka-go"
)

func msgAt(partition int, offset int64) kafkago.Message {
	return kafkago.Message{Topic: "t", Partition: partition, Offset: offset}
}

// track then complete, returning what the tracker says to commit.
func completeOne(t *offsetTracker, m kafkago.Message) (int64, bool) {
	got, ok := t.complete(m)
	if !ok {
		return -1, false
	}
	return got.Offset, true
}

// The core EV-4 property: a message that finishes while an earlier one is still
// running must not be committed.
func TestOffsetTracker_HoldsCommitWhileEarlierIsInFlight(t *testing.T) {
	tr := newOffsetTracker()
	for i := int64(1); i <= 3; i++ {
		tr.track(msgAt(0, i))
	}

	// 2 and 3 finish first. Neither may be committed: 1 is still running, and
	// committing either would declare 1 consumed.
	if off, ok := completeOne(tr, msgAt(0, 2)); ok {
		t.Errorf("committed offset %d while offset 1 was still in flight; "+
			"a crash here loses message 1 permanently", off)
	}
	if off, ok := completeOne(tr, msgAt(0, 3)); ok {
		t.Errorf("committed offset %d while offset 1 was still in flight", off)
	}

	// 1 finishes: the whole contiguous run 1,2,3 is now safe, and cumulative
	// commits mean that is one commit at the highest of them.
	off, ok := completeOne(tr, msgAt(0, 1))
	if !ok {
		t.Fatal("nothing committed after the blocking message finished; the run is stuck forever")
	}
	if off != 3 {
		t.Errorf("committed offset %d, want 3 (the top of the contiguous run 1,2,3)", off)
	}
	if n := tr.pendingCount(); n != 0 {
		t.Errorf("%d message(s) still held after the partition drained; the tracker leaks", n)
	}
}

// In-order completion must commit each message as it lands, not batch them up.
func TestOffsetTracker_InOrderCommitsEachTime(t *testing.T) {
	tr := newOffsetTracker()
	for i := int64(5); i <= 7; i++ {
		tr.track(msgAt(0, i))
	}
	for i := int64(5); i <= 7; i++ {
		off, ok := completeOne(tr, msgAt(0, i))
		if !ok || off != i {
			t.Fatalf("offset %d: committed (%d, %v), want (%d, true)", i, off, ok, i)
		}
	}
}

// Partitions are independent sequences. A stalled handler on one must not block
// commits on another — otherwise one slow partition halts the whole consumer.
func TestOffsetTracker_PartitionsAreIndependent(t *testing.T) {
	tr := newOffsetTracker()
	tr.track(msgAt(0, 1))
	tr.track(msgAt(0, 2))
	tr.track(msgAt(1, 1))

	// Partition 0 offset 1 never finishes. Partition 1 must still commit.
	if _, ok := completeOne(tr, msgAt(0, 2)); ok {
		t.Error("partition 0 committed with offset 1 still in flight")
	}
	off, ok := completeOne(tr, msgAt(1, 1))
	if !ok || off != 1 {
		t.Errorf("partition 1 committed (%d, %v), want (1, true): a stalled handler on "+
			"another partition must not block this one", off, ok)
	}
}

// A consumer group resumes at whatever offset it left off at, which is rarely 0.
// The first message seen defines where the partition's sequence starts; assuming
// 0 would leave every commit waiting on offsets that will never arrive.
func TestOffsetTracker_StartsAtFirstOffsetSeen(t *testing.T) {
	tr := newOffsetTracker()
	tr.track(msgAt(0, 4_000))
	off, ok := completeOne(tr, msgAt(0, 4_000))
	if !ok || off != 4_000 {
		t.Errorf("committed (%d, %v), want (4000, true): the tracker must resume from "+
			"the first offset it sees, not from zero", off, ok)
	}
}

// A consumer-group rebalance can revoke a partition and hand it back after
// another member has advanced it, so the next fetch arrives above where this
// tracker left off. With nothing in flight there is no run to protect, so the
// sequence must resync — otherwise every later completion waits on offsets that
// will never be fetched and the partition stops committing for good.
func TestOffsetTracker_ResyncsAfterPartitionResumesAhead(t *testing.T) {
	tr := newOffsetTracker()
	tr.track(msgAt(0, 1))
	if off, ok := completeOne(tr, msgAt(0, 1)); !ok || off != 1 {
		t.Fatalf("setup: committed (%d, %v), want (1, true)", off, ok)
	}

	// Rebalance: the partition comes back at 900, not 2.
	tr.track(msgAt(0, 900))
	off, ok := completeOne(tr, msgAt(0, 900))
	if !ok {
		t.Fatal("nothing committed after the partition resumed ahead; it is stuck permanently")
	}
	if off != 900 {
		t.Errorf("committed offset %d, want 900", off)
	}
}

// The resync must NOT fire while work is still in flight — that is the ordinary
// out-of-order case, and skipping ahead there is exactly the EV-4 bug.
func TestOffsetTracker_DoesNotResyncWhileInFlight(t *testing.T) {
	tr := newOffsetTracker()
	tr.track(msgAt(0, 1))
	tr.track(msgAt(0, 2)) // 1 is still in flight when 2 arrives

	if off, ok := completeOne(tr, msgAt(0, 2)); ok {
		t.Errorf("committed offset %d with offset 1 in flight: the resync path must not "+
			"trigger on a normal out-of-order arrival", off)
	}
}

// Committing an offset at or below one already committed would move the group
// backwards and replay everything above it.
func TestOffsetTracker_IgnoresAlreadyCommittedOffset(t *testing.T) {
	tr := newOffsetTracker()
	tr.track(msgAt(0, 1))
	tr.track(msgAt(0, 2))
	if off, ok := completeOne(tr, msgAt(0, 1)); !ok || off != 1 {
		t.Fatalf("setup: committed (%d, %v), want (1, true)", off, ok)
	}
	if off, ok := completeOne(tr, msgAt(0, 1)); ok {
		t.Errorf("re-committed offset %d, which would rewind the consumer group", off)
	}
}

// The tracker is called from one goroutine per message. Exactly one commit must
// cover the whole run, and it must be the highest offset — never more than one
// goroutine believing it holds the top.
func TestOffsetTracker_ConcurrentCompletionsCommitEachOffsetOnce(t *testing.T) {
	const n = 200
	tr := newOffsetTracker()
	for i := int64(1); i <= n; i++ {
		tr.track(msgAt(0, i))
	}

	var mu sync.Mutex
	var committed []int64
	var wg sync.WaitGroup
	for i := int64(1); i <= n; i++ {
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			if got, ok := completeOne(tr, msgAt(0, off)); ok {
				mu.Lock()
				committed = append(committed, got)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(committed) == 0 {
		t.Fatal("nothing was ever committed")
	}
	var max int64
	for _, c := range committed {
		if c > max {
			max = c
		}
	}
	if max != n {
		t.Errorf("highest committed offset = %d, want %d: the final run was never released", max, n)
	}
	if p := tr.pendingCount(); p != 0 {
		t.Errorf("%d message(s) still pending after every handler completed", p)
	}
}
