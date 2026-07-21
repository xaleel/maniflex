package redis

// Audit JB-3: the Redis job queue read only with XREADGROUP ">", which returns
// solely never-delivered messages. A worker that died after delivery but before
// Ack/Nack left its message in the group's pending entries list (PEL). Nothing
// ran XPENDING or XAUTOCLAIM, so that job was never redelivered — lost while the
// queue looked healthy — and the in-memory pending map Ack/Nack depends on had
// died with the process too.
//
// The fix reclaims PEL entries idle past ReclaimMinIdle via XAUTOCLAIM, folded
// into Dequeue so it fits the pull-based Source model; a live worker resets a
// running job's idle clock through RenewLease so its own long jobs are never
// reclaimed. There is no Redis server here, so the consumer path runs against a
// fake behind the streamOps seam (as events/redis does for EV-7).
//
//	go test ./jobs/redis/... -run TestReclaim

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/jobs"
)

// fakeEntry is one message in the fake's pending list.
type fakeEntry struct {
	id      string
	payload string
	idle    time.Duration // how long it has sat unacked
	acked   bool
}

// fakeOps models a consumer group's PEL and new-message queue in memory.
type fakeOps struct {
	mu sync.Mutex

	pel    []*fakeEntry // delivered-but-unacked, each with an idle age
	newMsg []*fakeEntry // never-delivered ("`>`") messages

	ensured    bool
	claimCalls int
	readCalls  int
	renewed    map[string]int // id → times its idle was reset via Claim
}

func newFakeOps() *fakeOps { return &fakeOps{renewed: map[string]int{}} }

func (f *fakeOps) EnsureGroup(context.Context, string, string) error {
	f.mu.Lock()
	f.ensured = true
	f.mu.Unlock()
	return nil
}

func (f *fakeOps) ReadGroup(_ context.Context, _, _, _ string, count int64, _ time.Duration) ([]goredis.XStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	var msgs []goredis.XMessage
	for len(f.newMsg) > 0 && int64(len(msgs)) < count {
		e := f.newMsg[0]
		f.newMsg = f.newMsg[1:]
		f.pel = append(f.pel, e) // delivery moves it into the PEL, idle 0
		e.idle = 0
		msgs = append(msgs, goredis.XMessage{ID: e.id, Values: map[string]any{"job": e.payload}})
	}
	if len(msgs) == 0 {
		return nil, goredis.Nil
	}
	return []goredis.XStream{{Stream: "s", Messages: msgs}}, nil
}

func (f *fakeOps) AutoClaim(_ context.Context, _, _, _ string, minIdle time.Duration, _ string, count int64) ([]goredis.XMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls++
	var msgs []goredis.XMessage
	for _, e := range f.pel {
		if int64(len(msgs)) >= count {
			break
		}
		if e.acked || e.idle < minIdle {
			continue
		}
		e.idle = 0 // reclaimed to us: idle resets
		msgs = append(msgs, goredis.XMessage{ID: e.id, Values: map[string]any{"job": e.payload}})
	}
	return msgs, "0-0", nil
}

func (f *fakeOps) Ack(_ context.Context, _, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.pel {
		if e.id == id {
			e.acked = true
		}
	}
	return nil
}

func (f *fakeOps) Claim(_ context.Context, _, _, _ string, ids []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var reset []string
	for _, id := range ids {
		for _, e := range f.pel {
			if e.id == id && !e.acked {
				e.idle = 0
				f.renewed[id]++
				reset = append(reset, id)
			}
		}
	}
	return reset, nil
}

func (f *fakeOps) pendingUnacked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.pel {
		if !e.acked {
			n++
		}
	}
	return n
}

// busWith wires a Queue onto a fake, with a short reclaim window so tests can
// express "idle long enough to reclaim" in milliseconds.
func queueWith(ops streamOps, minIdle time.Duration) *Queue {
	return &Queue{
		ops:            ops,
		stream:         "s",
		group:          "g",
		consumerID:     "test-consumer",
		reclaimMinIdle: minIdle,
		pending:        map[string]pendingEntry{},
		promoterStop:   make(chan struct{}),
	}
}

func jobPayload(t *testing.T, id, typ string) string {
	t.Helper()
	b, err := json.Marshal(jobs.Job{ID: id, Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The core JB-3 regression: a message abandoned in the PEL (idle past the
// threshold) must be returned by Dequeue even though there is nothing new to
// read. Before the fix, Dequeue read only ">" and never saw it.
func TestReclaim_AbandonedMessageIsRedelivered(t *testing.T) {
	ops := newFakeOps()
	ops.pel = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-a", "email"), idle: time.Minute}}
	q := queueWith(ops, 100*time.Millisecond)

	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 1 || got[0].ID != "job-a" {
		t.Fatalf("reclaim returned %+v, want the abandoned job-a", got)
	}
	// And it is tracked, so the worker's Ack can address it.
	if err := q.Ack(context.Background(), "job-a"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ops.pendingUnacked() != 0 {
		t.Errorf("job still pending after ack; a crashed worker's job would leak again")
	}
}

// A message that has NOT been idle long enough belongs to a worker that may
// still be alive — reclaiming it would double-run the job.
func TestReclaim_FreshlyDeliveredMessageIsNotReclaimed(t *testing.T) {
	ops := newFakeOps()
	ops.pel = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-a", "email"), idle: 5 * time.Millisecond}}
	q := queueWith(ops, time.Minute)

	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reclaimed a message idle %v under a %v window — a live worker's job would run twice",
			5*time.Millisecond, time.Minute)
	}
}

// Reclaim takes priority, but when nothing is reclaimable Dequeue must still
// deliver new work.
func TestReclaim_FallsThroughToNewMessages(t *testing.T) {
	ops := newFakeOps()
	ops.newMsg = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-new", "email")}}
	q := queueWith(ops, time.Minute)

	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 1 || got[0].ID != "job-new" {
		t.Fatalf("got %+v, want the new job", got)
	}
	if ops.claimCalls == 0 {
		t.Error("reclaim was never attempted; a crashed worker's backlog would never drain")
	}
	if ops.readCalls == 0 {
		t.Error("new messages were never read")
	}
}

// RenewLease resets the idle clock, so a job a live worker is still running is
// kept out of reclaim range no matter how long it runs.
func TestReclaim_RenewLeaseKeepsALiveJobFromBeingReclaimed(t *testing.T) {
	ops := newFakeOps()
	ops.newMsg = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-long", "email")}}
	q := queueWith(ops, 50*time.Millisecond)
	ctx := context.Background()

	got, err := q.Dequeue(ctx, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("initial dequeue: got %d err %v", len(got), err)
	}

	// The handler is still running: it ages toward the threshold, but each renew
	// resets it. Simulate several renew cycles with aging in between.
	for range 3 {
		ops.mu.Lock()
		for _, e := range ops.pel {
			e.idle = 40 * time.Millisecond // aged, but still under 50ms
		}
		ops.mu.Unlock()
		if err := q.RenewLease(ctx, "job-long", time.Minute); err != nil {
			t.Fatalf("renew: %v", err)
		}
	}

	if ops.renewed["1-1"] != 3 {
		t.Errorf("idle reset %d times, want 3", ops.renewed["1-1"])
	}

	// Now push it just over the threshold and confirm a second worker's reclaim
	// would only fire once renewal stops.
	ops.mu.Lock()
	for _, e := range ops.pel {
		e.idle = 60 * time.Millisecond
	}
	ops.mu.Unlock()
	reclaimed := q.reclaim(ctx, 10)
	if len(reclaimed) != 1 {
		t.Errorf("after renewal stopped and idle passed the window, reclaim returned %d, want 1", len(reclaimed))
	}
}

// A reclaimed message that will not decode must be acked and dropped, not
// retried forever — a retry decodes the same bytes and fails identically.
func TestReclaim_MalformedMessageIsAckedAndSkipped(t *testing.T) {
	ops := newFakeOps()
	ops.pel = []*fakeEntry{
		{id: "1-1", payload: "{not json", idle: time.Minute},
		{id: "1-2", payload: jobPayload(t, "job-ok", "email"), idle: time.Minute},
	}
	q := queueWith(ops, time.Millisecond)

	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 1 || got[0].ID != "job-ok" {
		t.Fatalf("got %+v, want only the decodable job", got)
	}
	// The poison entry must be acked so it stops coming back.
	ops.mu.Lock()
	var poison *fakeEntry
	for _, e := range ops.pel {
		if e.id == "1-1" {
			poison = e
		}
	}
	ops.mu.Unlock()
	if poison == nil || !poison.acked {
		t.Error("undecodable reclaimed message was not acked; it would be reclaimed every sweep forever")
	}
}

// Each delivery increments Attempts, so a job that is repeatedly reclaimed
// because it keeps failing eventually reaches MaxRetry and dies — the reclaim
// loop cannot run forever on a poison-but-decodable job.
func TestReclaim_IncrementsAttemptsSoRetriesAreBounded(t *testing.T) {
	ops := newFakeOps()
	ops.pel = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-a", "email"), idle: time.Minute}}
	q := queueWith(ops, time.Millisecond)

	got := q.reclaim(context.Background(), 10)
	if len(got) != 1 {
		t.Fatalf("reclaim returned %d, want 1", len(got))
	}
	if got[0].Attempts != 1 {
		t.Errorf("Attempts = %d after one reclaim, want 1 — the worker dead-letters at MaxRetry", got[0].Attempts)
	}
}

// The default ConsumerID must be unique per process, not the old fixed
// "worker-0" that made every worker one consumer sharing a single PEL.
func TestReclaim_DefaultConsumerIDIsUnique(t *testing.T) {
	name := defaultConsumerName()
	if name == "" || name == "worker-0" {
		t.Errorf("default consumer ID = %q, want a unique per-process name", name)
	}
	if got := New(nil, Options{}).consumerID; got == "worker-0" {
		t.Errorf("New defaulted ConsumerID to the shared %q", got)
	}
}

// Requeue must ack the current stream message (removing it from the PEL) and
// drop it from the in-memory pending map before re-enqueuing, so it is not both
// reclaimable AND re-enqueued. The re-enqueue itself goes through the raw client
// and needs a Redis server, so here it is expected to fail fast; the ack and the
// pending-map removal are what this asserts (the recovery-relevant half). The
// full re-enqueue is read-verified, as with the rest of this no-server suite.
func TestReclaim_RequeueAcksAndUntracksBeforeReenqueue(t *testing.T) {
	ops := newFakeOps()
	ops.pel = []*fakeEntry{{id: "1-1", payload: jobPayload(t, "job-a", "email"), idle: time.Minute}}
	q := queueWith(ops, time.Millisecond)
	// A client that fails to dial fast, so the re-enqueue errors instead of
	// panicking on a nil client — no Redis is present.
	q.client = goredis.NewClient(&goredis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond, MaxRetries: -1,
	})
	defer q.client.Close()

	ctx := context.Background()
	got, err := q.Dequeue(ctx, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("dequeue: got %d err %v", len(got), err)
	}

	// Requeue returns the enqueue error (no server); the ack must have happened
	// first regardless.
	_ = q.Requeue(ctx, got[0], time.Second)

	if ops.pendingUnacked() != 0 {
		t.Error("Requeue did not ack the old stream message; it stays in the PEL and can be reclaimed again")
	}
	q.pendingMu.Lock()
	_, still := q.pending[got[0].ID]
	q.pendingMu.Unlock()
	if still {
		t.Error("Requeue left the job in the in-memory pending map")
	}
}

// Anti-vacuity: with no pending and no new messages, Dequeue returns empty and
// still attempts a reclaim (so recovery is always in the loop) — it must not
// spuriously invent jobs.
func TestReclaim_EmptyQueueReturnsNothing(t *testing.T) {
	ops := newFakeOps()
	q := queueWith(ops, time.Minute)

	got, err := q.Dequeue(context.Background(), 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d jobs from an empty queue, want 0", len(got))
	}
	if ops.claimCalls == 0 {
		t.Error("reclaim was not attempted on an empty queue")
	}
}
