package redis

// Audit JB-8: the delayed-job promoter ran a plain ZRANGEBYSCORE + per-member
// XADD/ZREM pipeline on every instance, so N replicas each promoted the same due
// member — the delayed job ran N times — and a dropped connection could leave a
// member half-moved. Promotion is now a single server-side script (promoteOps /
// promoteScript), which Redis runs atomically and single-threaded.
//
// No live Redis here, so these tests drive promoteDue through a fake promoteOps
// whose PromoteDue is atomic (guarded by one mutex), exactly the contract the
// EVAL provides. They prove promoteDue delegates to that atomic seam rather than
// doing its own read-then-write, and that under the contract concurrent promoters
// never move a member twice. The production PromoteDue being a single EVAL — and
// thus atomic by Redis's definition — is read-verified in promote.go.
//
//	go test ./jobs/redis/ -run TestPromote

import (
	"context"
	"sort"
	"sync"
	"testing"
)

// fakePromoter models the delayed set and the stream in memory, moving due
// members from one to the other under a single lock — the atomicity the real
// EVAL has on the server. Several Queues may share one fake to model several
// instances promoting against the same Redis.
type fakePromoter struct {
	mu     sync.Mutex
	zset   map[string]int64 // member → score (unix ms)
	stream []string         // promoted members, in XADD order
	calls  int
	err    error
}

func newFakePromoter() *fakePromoter {
	return &fakePromoter{zset: map[string]int64{}}
}

func (f *fakePromoter) PromoteDue(_ context.Context, _, _ string, nowMs, count int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	type due struct {
		member string
		score  int64
	}
	var ready []due
	for m, s := range f.zset {
		if s <= nowMs {
			ready = append(ready, due{m, s})
		}
	}
	// Lowest score (soonest due) first, mirroring ZRANGEBYSCORE order, so a count
	// cap takes the members that have waited longest.
	sort.Slice(ready, func(i, j int) bool { return ready[i].score < ready[j].score })

	var n int64
	for _, d := range ready {
		if n >= count {
			break
		}
		delete(f.zset, d.member) // ZREM
		f.stream = append(f.stream, d.member)
		n++
	}
	return n, nil
}

// queueForPromote builds a Queue wired to a fake promoter, with no client — so a
// regression that promoted via q.client instead of the seam would panic here.
func queueForPromote(p promoteOps) *Queue {
	return &Queue{promoter: p, delayed: "d", stream: "s"}
}

// promoteDue moves members due at or before now and leaves the rest.
func TestPromote_MovesDueLeavesFuture(t *testing.T) {
	f := newFakePromoter()
	f.zset["past"] = 1         // long overdue
	f.zset["future"] = 1 << 62 // far in the future
	q := queueForPromote(f)

	q.promoteDue(context.Background())

	if len(f.stream) != 1 || f.stream[0] != "past" {
		t.Fatalf("stream = %v, want [past]", f.stream)
	}
	if _, stillDelayed := f.zset["future"]; !stillDelayed {
		t.Errorf("a not-yet-due member was promoted")
	}
	if _, stillDelayed := f.zset["past"]; stillDelayed {
		t.Errorf("a promoted member was left in the delayed set (would be promoted again)")
	}
}

// The headline JB-8 property: two instances promoting the same due members move
// each member exactly once, not once per instance.
func TestPromote_NoDoublePromoteAcrossInstances(t *testing.T) {
	f := newFakePromoter()
	const members = 50
	for i := range members {
		f.zset[jobPayload(t, itoa(i), "email")] = 1 // all due
	}
	q1, q2 := queueForPromote(f), queueForPromote(f)

	var wg sync.WaitGroup
	for _, q := range []*Queue{q1, q2, q1, q2} {
		wg.Add(1)
		go func(q *Queue) { defer wg.Done(); q.promoteDue(context.Background()) }(q)
	}
	wg.Wait()

	if len(f.stream) != members {
		t.Fatalf("promoted %d members across instances, want exactly %d (no duplicates, no losses)", len(f.stream), members)
	}
	seen := map[string]bool{}
	for _, m := range f.stream {
		if seen[m] {
			t.Errorf("member promoted twice: %q", m)
		}
		seen[m] = true
	}
	if len(f.zset) != 0 {
		t.Errorf("%d due members were left unpromoted", len(f.zset))
	}
}

// promoteDue caps a tick at promoteBatch members; the rest wait for the next tick.
func TestPromote_RespectsBatchCap(t *testing.T) {
	f := newFakePromoter()
	for i := range promoteBatch + 10 {
		f.zset[itoa(i)] = int64(i + 1) // all due, distinct scores so the cap is deterministic
	}
	q := queueForPromote(f)

	q.promoteDue(context.Background())

	if len(f.stream) != promoteBatch {
		t.Fatalf("promoted %d in one tick, want the batch cap %d", len(f.stream), promoteBatch)
	}
	if len(f.zset) != 10 {
		t.Errorf("%d members left, want 10 to carry to the next tick", len(f.zset))
	}
}

// A promote error is swallowed (best-effort, self-healing next tick): promoteDue
// must not panic or otherwise disturb the loop.
func TestPromote_ErrorIsBestEffort(t *testing.T) {
	f := newFakePromoter()
	f.err = context.DeadlineExceeded
	q := queueForPromote(f)

	q.promoteDue(context.Background()) // must not panic
	if f.calls != 1 {
		t.Errorf("promoteDue did not call the seam exactly once (calls=%d)", f.calls)
	}
}

// racyPromoter models the OLD non-atomic promoter: it snapshots the due set,
// then (after every instance has snapshotted) unconditionally appends each member
// it saw to the stream — the ZRANGEBYSCORE-then-XADD/ZREM pipeline that N replicas
// each ran against the same members. The snapshotted barrier forces the exact
// interleave the atomic script rules out, deterministically, so the anti-vacuity
// test below does not depend on goroutine timing.
type racyPromoter struct {
	mu          sync.Mutex
	zset        map[string]int64
	stream      []string
	snapshotted sync.WaitGroup
}

func (f *racyPromoter) PromoteDue(_ context.Context, _, _ string, nowMs, _ int64) (int64, error) {
	f.mu.Lock()
	var due []string
	for m, s := range f.zset {
		if s <= nowMs {
			due = append(due, m)
		}
	}
	f.mu.Unlock()

	f.snapshotted.Done()
	f.snapshotted.Wait() // every instance has now read the same due set

	f.mu.Lock()
	for _, m := range due {
		f.stream = append(f.stream, m) // unconditional XADD, as the old pipeline did
		delete(f.zset, m)              // ZREM (idempotent)
	}
	f.mu.Unlock()
	return int64(len(due)), nil
}

// Anti-vacuity: the non-atomic promoter DOES promote each member twice across two
// instances, so TestPromote_NoDoublePromoteAcrossInstances genuinely tests the
// atomicity — it is not passing simply because the fake never duplicates.
func TestPromote_NonAtomicPromoterDuplicates(t *testing.T) {
	f := &racyPromoter{zset: map[string]int64{"a": 1, "b": 1, "c": 1}}
	f.snapshotted.Add(2)
	q1, q2 := queueForPromote(f), queueForPromote(f)

	var wg sync.WaitGroup
	for _, q := range []*Queue{q1, q2} {
		wg.Add(1)
		go func(q *Queue) { defer wg.Done(); q.promoteDue(context.Background()) }(q)
	}
	wg.Wait()

	if len(f.stream) != 6 {
		t.Fatalf("non-atomic promoter moved %d, want 6 (each of 3 members twice) — "+
			"the exclusivity test would not catch a non-atomic promoter", len(f.stream))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
