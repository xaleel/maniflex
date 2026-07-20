package events_test

// Audit EV-2 (High): Dedupe records the event ID on handler *entry* — Seen is an
// atomic check-and-record — so when the wrapped handler returns a transient
// error, DeliverWithRetry's second attempt finds the ID already recorded, drops
// the event and returns nil. The retry is treated as a successful delivery: the
// event is never processed, never dead-lettered, and nothing is logged.
//
// The docs recommend Dedupe(store)(handler) "with every at-least-once broker
// adapter", so following the documented pattern loses an event on the first
// transient failure — the exact failure the retry exists to absorb.
//
//	go test ./tests/events/... -run TestDedupeRetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
)

// flakyHandler fails its first n calls, then succeeds. It records how many times
// the *inner* handler actually ran, which is what EV-2 is about.
type flakyHandler struct {
	mu       sync.Mutex
	failFor  int
	calls    int
	saw      []string
	finished bool
}

func (h *flakyHandler) handle(_ context.Context, e events.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.saw = append(h.saw, e.ID)
	if h.calls <= h.failFor {
		return errors.New("transient failure")
	}
	h.finished = true
	return nil
}

func (h *flakyHandler) state() (calls int, finished bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.finished
}

// noBackoff keeps the retry loop instant; the timing is not what is under test.
func noBackoff(int) time.Duration { return 0 }

// TestDedupeRetry_TransientFailureIsRetried is the EV-2 regression. One
// transient failure, three attempts available: the handler must run again and
// the event must end up processed.
func TestDedupeRetry_TransientFailureIsRetried(t *testing.T) {
	store := events.NewInMemoryDedupeStore(10, time.Hour)
	h := &flakyHandler{failFor: 1}

	sub := events.Subscription{
		Handler:  events.Dedupe(store)(h.handle),
		MaxRetry: 3,
		Backoff:  noBackoff,
	}
	pub := &capturePub{}

	events.DeliverWithRetry(context.Background(), pub, sub,
		events.Event{ID: "ev-transient", Type: "x", Time: time.Now()})

	calls, finished := h.state()
	if calls < 2 {
		t.Errorf("handler ran %d time(s): the retry was swallowed by the dedupe store, "+
			"so the event was silently dropped after one transient failure", calls)
	}
	if !finished {
		t.Error("event was never processed successfully — it is lost, with no DLQ and no log")
	}
}

// A handler that never succeeds must still exhaust its attempts and reach the
// DLQ. Under EV-2 attempt 2 returned nil, so delivery "succeeded" and nothing
// was ever dead-lettered.
func TestDedupeRetry_PermanentFailureStillDeadLetters(t *testing.T) {
	store := events.NewInMemoryDedupeStore(10, time.Hour)
	h := &flakyHandler{failFor: 1000}

	sub := events.Subscription{
		Handler:  events.Dedupe(store)(h.handle),
		MaxRetry: 2,
		Backoff:  noBackoff,
		DLQ:      "x.dlq",
	}
	pub := &capturePub{}

	events.DeliverWithRetry(context.Background(), pub, sub,
		events.Event{ID: "ev-permanent", Type: "x", Time: time.Now()})

	calls, _ := h.state()
	if calls != 3 {
		t.Errorf("handler ran %d time(s), want 3 (MaxRetry=2 → 3 attempts)", calls)
	}
	got := pub.published()
	if len(got) != 1 {
		t.Fatalf("published %d events, want exactly 1 dead-letter", len(got))
	}
	if got[0].Type != "x.dlq" {
		t.Errorf("dead-letter type = %q, want %q", got[0].Type, "x.dlq")
	}
}

// The SQL store is the one that runs in production, and its Release is a
// different statement from the in-memory map delete — a fix applied to only one
// store passes every test above.
func TestDedupeRetry_SQLStoreReleasesOnFailure(t *testing.T) {
	db := openDedupeDB(t)
	store := events.NewSQLDedupeStore(db, "sqlite")
	h := &flakyHandler{failFor: 1}

	sub := events.Subscription{
		Handler:  events.Dedupe(store)(h.handle),
		MaxRetry: 3,
		Backoff:  noBackoff,
	}
	events.DeliverWithRetry(context.Background(), &capturePub{}, sub,
		events.Event{ID: "sql-transient", Type: "x", Time: time.Now()})

	if calls, finished := h.state(); calls < 2 || !finished {
		t.Errorf("SQL store: handler ran %d time(s), finished=%v; want the retry to "+
			"re-process after the claim was released", calls, finished)
	}

	// The successful attempt must leave the claim in place.
	seen, err := store.Seen(context.Background(), "sql-transient")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("after successful processing the ID is not recorded; a redelivery would re-run the handler")
	}
}

// noReleaseStore is what a third-party DedupeStore looks like from the package's
// side: it satisfies DedupeStore and nothing more. Embedding the *interface*
// rather than the concrete store is what keeps Release from being promoted.
type noReleaseStore struct{ events.DedupeStore }

// The documented degradation. There is no correct fallback — the claim cannot be
// undone — so this pins the behaviour rather than blessing it: a store without
// Release still loses the event, which is why Dedupe warns when it is wrapped.
// If this ever starts passing, the fallback has quietly changed and the warning
// in Dedupe's godoc is wrong.
func TestDedupeRetry_StoreWithoutReleaseStillDropsTheRetry(t *testing.T) {
	store := noReleaseStore{events.NewInMemoryDedupeStore(10, time.Hour)}
	if _, ok := any(store).(events.DedupeReleaser); ok {
		t.Fatal("precondition: noReleaseStore must NOT implement DedupeReleaser, " +
			"or this test silently exercises the fast path instead of the fallback")
	}
	h := &flakyHandler{failFor: 1}

	sub := events.Subscription{
		Handler:  events.Dedupe(store)(h.handle),
		MaxRetry: 3,
		Backoff:  noBackoff,
	}
	events.DeliverWithRetry(context.Background(), &capturePub{}, sub,
		events.Event{ID: "no-release", Type: "x", Time: time.Now()})

	if calls, _ := h.state(); calls != 1 {
		t.Errorf("handler ran %d time(s), want 1: a store that cannot release "+
			"cannot support retry, and Dedupe's godoc says so", calls)
	}
}

// Anti-vacuity, and the property the fix must not trade away: a genuine
// redelivery of an already-processed event is still dropped. A fix that simply
// stopped recording IDs would pass both tests above and defeat the whole point
// of the store.
func TestDedupeRetry_SucceededEventIsStillDeduped(t *testing.T) {
	store := events.NewInMemoryDedupeStore(10, time.Hour)
	h := &flakyHandler{failFor: 1}

	sub := events.Subscription{
		Handler:  events.Dedupe(store)(h.handle),
		MaxRetry: 3,
		Backoff:  noBackoff,
	}
	pub := &capturePub{}
	e := events.Event{ID: "ev-redelivered", Type: "x", Time: time.Now()}

	// First delivery: fails once, then succeeds — 2 handler runs.
	events.DeliverWithRetry(context.Background(), pub, sub, e)
	after, _ := h.state()

	// The broker redelivers the same event. It was processed; it must not run again.
	events.DeliverWithRetry(context.Background(), pub, sub, e)

	final, _ := h.state()
	if final != after {
		t.Errorf("handler ran %d more time(s) on redelivery of an already-processed "+
			"event; dedupe must still suppress it", final-after)
	}
}
