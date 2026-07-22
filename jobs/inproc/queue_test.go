package inproc

// Audit JB-15: Cancel was the one terminal path that did not remove its entry —
// Ack, Dead and an exhausted Nack all do — so every cancellation leaked an entry
// for the life of the process, and each one stayed in the slice Dequeue walks on
// every call.
//
// The same class, found while reading and fixed with it: releaseGroup decremented
// runningKeys but left the key behind at zero, so the map kept one entry per
// distinct GroupKey ever seen. Group keys are usually high-cardinality (a user or
// invoice id), so that grew without bound on exactly the workloads that use them.
//
//	go test ./jobs/inproc/ -run 'TestCancel|TestGroupKey'

import (
	"context"
	"fmt"
	"testing"

	"github.com/xaleel/maniflex/jobs"
)

func mustEnqueue(t *testing.T, q *Queue, j jobs.Job) string {
	t.Helper()
	id, err := q.Enqueue(context.Background(), j)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// sizes reports the queue's retained state. Both must shrink as jobs finish.
func sizes(q *Queue) (entries, byID, groups int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries), len(q.byID), len(q.runningKeys)
}

func TestCancel_RemovesTheEntry(t *testing.T) {
	ctx := context.Background()
	q := New()
	keep := mustEnqueue(t, q, jobs.Job{Type: "a"})
	drop := mustEnqueue(t, q, jobs.Job{Type: "b"})

	if err := q.Cancel(ctx, drop); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	entries, byID, _ := sizes(q)
	if entries != 1 || byID != 1 {
		t.Errorf("after cancelling 1 of 2: entries=%d byID=%d, want 1 and 1", entries, byID)
	}
	if _, err := q.Get(ctx, drop); err == nil {
		t.Error("cancelled job is still retained")
	}
	// The survivor is untouched and still claimable.
	claimed, err := q.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != keep {
		t.Errorf("dequeue returned %v, want only the uncancelled job %s", claimed, keep)
	}
}

// The growth property: a queue that has served many cancellations retains nothing.
func TestCancel_LeavesNoResidue(t *testing.T) {
	ctx := context.Background()
	q := New()
	const n = 1000
	for i := range n {
		id := mustEnqueue(t, q, jobs.Job{Type: fmt.Sprintf("t%d", i)})
		if err := q.Cancel(ctx, id); err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
	}

	entries, byID, _ := sizes(q)
	if entries != 0 || byID != 0 {
		t.Errorf("after %d enqueue+cancel cycles: entries=%d byID=%d, want 0 and 0 — "+
			"cancellations accumulate for the life of the process", n, entries, byID)
	}
}

// Anti-regression: a job already handed to a worker cannot be cancelled, and the
// refusal must not remove it.
func TestCancel_RunningJobIsRefused(t *testing.T) {
	ctx := context.Background()
	q := New()
	id := mustEnqueue(t, q, jobs.Job{Type: "a"})
	if _, err := q.Dequeue(ctx, 1); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	if err := q.Cancel(ctx, id); err == nil {
		t.Error("cancelling a running job was allowed")
	}
	if entries, byID, _ := sizes(q); entries != 1 || byID != 1 {
		t.Errorf("refused cancel dropped the running job: entries=%d byID=%d", entries, byID)
	}
}

func TestCancel_UnknownIDErrors(t *testing.T) {
	if err := New().Cancel(context.Background(), "nope"); err == nil {
		t.Error("cancelling an unknown id did not error")
	}
}

// The sibling leak: a key nothing is running under is forgotten, not kept at zero.
func TestGroupKey_ReleasedKeysAreForgotten(t *testing.T) {
	ctx := context.Background()
	q := New()
	const n = 500
	for i := range n {
		mustEnqueue(t, q, jobs.Job{Type: "a", GroupKey: fmt.Sprintf("key-%d", i)})
	}

	claimed, err := q.Dequeue(ctx, n)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(claimed) != n {
		t.Fatalf("claimed %d of %d distinct-key jobs", len(claimed), n)
	}
	for _, j := range claimed {
		if err := q.Ack(ctx, j.ID); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}

	entries, byID, groups := sizes(q)
	if groups != 0 {
		t.Errorf("runningKeys retained %d keys after every job finished, want 0 — "+
			"one entry per distinct GroupKey ever seen accumulates for the life of "+
			"the process", groups)
	}
	if entries != 0 || byID != 0 {
		t.Errorf("entries=%d byID=%d after acking everything, want 0 and 0", entries, byID)
	}
}

// Anti-over-reach: forgetting a released key must not weaken the serialisation it
// exists for — one job per key at a time, and the next one runs once it is free.
func TestGroupKey_SerialisationStillHolds(t *testing.T) {
	ctx := context.Background()
	q := New()
	mustEnqueue(t, q, jobs.Job{Type: "a", GroupKey: "g"})
	mustEnqueue(t, q, jobs.Job{Type: "a", GroupKey: "g"})

	first, err := q.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("claimed %d jobs sharing one GroupKey, want 1", len(first))
	}
	// The second is blocked while the first holds the key.
	if again, _ := q.Dequeue(ctx, 10); len(again) != 0 {
		t.Fatalf("claimed %d more while the key was held, want 0", len(again))
	}

	if err := q.Ack(ctx, first[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	second, err := q.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(second) != 1 {
		t.Errorf("claimed %d after the key was released, want 1 — releasing the key "+
			"must hand it to the next job", len(second))
	}
}
