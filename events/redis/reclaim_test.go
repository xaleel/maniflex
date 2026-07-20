package redis

// Audit EV-7: the consumer read with XREADGROUP ">" only, which returns
// never-delivered messages. A consumer that died mid-delivery left its messages
// in the group's pending list (PEL) — unacknowledged and never redelivered, so
// effectively lost while the system looked perfectly healthy. Nothing ever ran
// XPENDING or XAUTOCLAIM to reclaim them.
//
// It compounded: the consumer name was "worker-<unix-nano>", fresh on every
// process start, and Redis never removes consumers from a group. So a restart
// could not reclaim even its own pending messages, and every deploy added a
// permanent consumer entry holding whatever that process had in flight.
//
// There is no Redis server here, so the consumer runs against a fake behind the
// streamOps seam. What that seam buys is real coverage of the parts most likely
// to be wrong: that reclaim happens at all, that a reclaimed message is
// delivered AND acked, that the cursor loop terminates, and that cancellation
// stops it.
//
//	go test ./events/redis/...

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/events"
)

// fakeOps implements streamOps over in-memory state.
type fakeOps struct {
	mu sync.Mutex

	// pending is what AutoClaim will hand back, keyed by the cursor page it is
	// returned on. pages[i] is returned for the i-th AutoClaim call of a sweep.
	pages [][]goredis.XMessage
	// cursors[i] is the cursor returned alongside pages[i].
	cursors []string

	claimCalls   int
	acked        []string
	claimedWith  []claimArgs
	groupsMade   []string
	readCalls    int
	autoClaimErr error
}

type claimArgs struct {
	consumer string
	minIdle  time.Duration
	start    string
}

func (f *fakeOps) EnsureGroup(_ context.Context, stream, group string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupsMade = append(f.groupsMade, stream+"/"+group)
	return nil
}

// ReadGroup never returns anything: these tests are about the reclaim path, and
// a read that returned messages would make it ambiguous which path delivered
// them. It blocks briefly so the consumer loop does not spin.
func (f *fakeOps) ReadGroup(ctx context.Context, _, _, _ string, _ int64, block time.Duration) ([]goredis.XStream, error) {
	f.mu.Lock()
	f.readCalls++
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(block):
	}
	return nil, goredis.Nil
}

func (f *fakeOps) AutoClaim(_ context.Context, _, _, consumer string, minIdle time.Duration, start string, _ int64) ([]goredis.XMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimedWith = append(f.claimedWith, claimArgs{consumer: consumer, minIdle: minIdle, start: start})
	if f.autoClaimErr != nil {
		return nil, "", f.autoClaimErr
	}
	i := f.claimCalls
	f.claimCalls++
	if i >= len(f.pages) {
		return nil, "0-0", nil // nothing left to claim
	}
	return f.pages[i], f.cursors[i], nil
}

func (f *fakeOps) Ack(_ context.Context, _, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, id)
	return nil
}

func (f *fakeOps) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acked...)
}

func (f *fakeOps) claims() []claimArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]claimArgs(nil), f.claimedWith...)
}

// msg builds a stream message carrying an event of the given type.
func msg(id, eventType string) goredis.XMessage {
	payload, _ := json.Marshal(events.Event{ID: "ev-" + id, Type: eventType, Time: time.Now()})
	return goredis.XMessage{ID: id, Values: map[string]any{"payload": string(payload)}}
}

// busWith returns a Bus wired to ops, with reclaim tuned to fire immediately.
func busWith(ops streamOps) *Bus {
	return &Bus{
		ops:    ops,
		prefix: "test",
		opts: Options{
			ConsumerName:  "test-consumer",
			ClaimMinIdle:  time.Minute,
			ClaimInterval: 10 * time.Millisecond,
		},
	}
}

// runFor starts the consumer, lets it work, then cancels and waits.
func runFor(t *testing.T, b *Bus, sub events.Subscription, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.runConsumer(ctx, "test:stream", sub)
	}()
	time.Sleep(d)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runConsumer did not return after cancel; the reclaim goroutine is not honouring ctx")
	}
}

func handlerRecording(got *[]string, mu *sync.Mutex) events.Handler {
	return func(_ context.Context, e events.Event) error {
		mu.Lock()
		*got = append(*got, e.ID)
		mu.Unlock()
		return nil
	}
}

// subWith builds a Subscription with the defaults Subscribe applies. These tests
// call runConsumer directly, and without them Concurrency is 0 — which makes the
// worker semaphore unbuffered, so every dispatch blocks and then bails on
// cancellation, and the whole suite fails for a reason unrelated to reclaim.
func subWith(patterns []string, h events.Handler) events.Subscription {
	return events.Subscription{
		Patterns:    patterns,
		Group:       "g",
		Handler:     h,
		Concurrency: 1,
		MaxRetry:    1,
		Backoff:     func(int) time.Duration { return 0 },
	}
}

func noopHandler(context.Context, events.Event) error { return nil }

// The EV-7 regression: a message abandoned in the PEL must be reclaimed,
// delivered to the handler, and acked.
func TestReclaim_AbandonedMessageIsDeliveredAndAcked(t *testing.T) {
	ops := &fakeOps{
		pages:   [][]goredis.XMessage{{msg("1-1", "invoice.created")}},
		cursors: []string{"0-0"},
	}
	var mu sync.Mutex
	var got []string

	runFor(t, busWith(ops), subWith([]string{"invoice.*"}, handlerRecording(&got, &mu)), 150*time.Millisecond)

	mu.Lock()
	delivered := append([]string(nil), got...)
	mu.Unlock()

	if len(delivered) == 0 {
		t.Fatal("a message pending in the PEL was never reclaimed: nothing runs XAUTOCLAIM, " +
			"so a crashed consumer's messages are never redelivered")
	}
	if delivered[0] != "ev-1-1" {
		t.Errorf("delivered %q, want %q", delivered[0], "ev-1-1")
	}

	acked := ops.ackedIDs()
	if len(acked) == 0 || acked[0] != "1-1" {
		t.Errorf("acked = %v, want the reclaimed message 1-1: without the ack it stays "+
			"pending and is reclaimed again on every sweep, forever", acked)
	}
}

// The claim must use this consumer's own name and the configured idle floor.
// Claiming with too small an idle window steals work from a consumer that is
// still running it, turning the fix into duplicate delivery.
func TestReclaim_UsesConfiguredIdleAndConsumerName(t *testing.T) {
	ops := &fakeOps{}
	b := busWith(ops)
	b.opts.ClaimMinIdle = 90 * time.Second

	runFor(t, b, subWith([]string{"*"}, noopHandler), 80*time.Millisecond)

	claims := ops.claims()
	if len(claims) == 0 {
		t.Fatal("XAUTOCLAIM was never called")
	}
	if claims[0].consumer != "test-consumer" {
		t.Errorf("claimed as %q, want the configured consumer name", claims[0].consumer)
	}
	if claims[0].minIdle != 90*time.Second {
		t.Errorf("min idle = %v, want 90s: claiming sooner steals messages a live consumer "+
			"is still working on", claims[0].minIdle)
	}
	if claims[0].start != "0-0" {
		t.Errorf("first scan started at %q, want %q", claims[0].start, "0-0")
	}
}

// A multi-page scan must follow the cursor rather than re-reading page one.
func TestReclaim_FollowsCursorAcrossPages(t *testing.T) {
	ops := &fakeOps{
		pages: [][]goredis.XMessage{
			{msg("1-1", "x.created")},
			{msg("2-2", "x.created")},
		},
		cursors: []string{"1-1", "0-0"},
	}
	var mu sync.Mutex
	var got []string

	runFor(t, busWith(ops), subWith([]string{"x.*"}, handlerRecording(&got, &mu)), 150*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("delivered %v, want both pages: the scan is not following the returned cursor", got)
	}
	claims := ops.claims()
	if len(claims) < 2 || claims[1].start != "1-1" {
		t.Errorf("second call started at %q, want the cursor %q from the first",
			claims[1].start, "1-1")
	}
}

// A server that keeps handing back a non-terminal cursor must not spin forever
// inside a goroutine nobody is watching. The sweep is bounded and resumes next
// tick.
func TestReclaim_BoundsASweepThatNeverTerminates(t *testing.T) {
	pages := make([][]goredis.XMessage, 0, 500)
	cursors := make([]string, 0, 500)
	for i := range 500 {
		pages = append(pages, nil)
		cursors = append(cursors, "never-ends") // never "0-0"
		_ = i
	}
	ops := &fakeOps{pages: pages, cursors: cursors}

	// runFor's own timeout is the assertion: an unbounded sweep never returns.
	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 80*time.Millisecond)

	if n := len(ops.claims()); n > maxClaimPages*20 {
		t.Errorf("%d XAUTOCLAIM calls in 80ms — the sweep is not bounded", n)
	}
}

// An error from XAUTOCLAIM is transient (a failover, a blip). It must end the
// sweep quietly and leave the ticker running, not kill the reclaimer.
func TestReclaim_SurvivesAutoClaimError(t *testing.T) {
	ops := &fakeOps{autoClaimErr: goredis.ErrClosed}

	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 80*time.Millisecond)

	if n := len(ops.claims()); n < 2 {
		t.Errorf("XAUTOCLAIM called %d time(s) after an error; the reclaimer gave up "+
			"permanently on a transient failure", n)
	}
}

// Anti-vacuity for the pattern filter: a reclaimed message that does not match
// the subscription must still be acked, or it is reclaimed on every sweep
// forever — but it must not reach the handler.
func TestReclaim_NonMatchingMessageIsAckedNotDelivered(t *testing.T) {
	ops := &fakeOps{
		pages:   [][]goredis.XMessage{{msg("9-9", "order.created")}},
		cursors: []string{"0-0"},
	}
	var mu sync.Mutex
	var got []string

	runFor(t, busWith(ops), subWith([]string{"invoice.*"}, handlerRecording(&got, &mu)), 120*time.Millisecond)

	mu.Lock()
	delivered := len(got)
	mu.Unlock()
	if delivered != 0 {
		t.Errorf("handler saw %d non-matching message(s)", delivered)
	}
	if acked := ops.ackedIDs(); len(acked) == 0 {
		t.Error("a non-matching reclaimed message was not acked; it will be reclaimed on every sweep forever")
	}
}

// ── consumer identity ────────────────────────────────────────────────────────

// The old name embedded time.Now().UnixNano(), so no two starts of the same
// worker shared an identity — and Redis never removes consumers from a group.
func TestDefaultConsumerName_IsStableAcrossCalls(t *testing.T) {
	a, b := defaultConsumerName(), defaultConsumerName()
	if a != b {
		t.Errorf("defaultConsumerName() returned %q then %q: an unstable name adds a "+
			"permanent consumer entry to every group on every restart, and orphans "+
			"whatever that process had pending", a, b)
	}
	if !strings.HasPrefix(a, "maniflex-") {
		t.Errorf("name = %q, want a maniflex- prefix so it is identifiable in XINFO CONSUMERS", a)
	}
}

func TestNew_OptionsDefaultAndOverride(t *testing.T) {
	b := New(nil, "p")
	if b.opts.ConsumerName == "" || b.opts.ClaimMinIdle != defaultClaimMinIdle ||
		b.opts.ClaimInterval != defaultClaimInterval {
		t.Errorf("defaults not applied: %+v", b.opts)
	}

	b = New(nil, "p", Options{ConsumerName: "mine", ClaimMinIdle: time.Second, ClaimInterval: 2 * time.Second})
	if b.opts.ConsumerName != "mine" || b.opts.ClaimMinIdle != time.Second || b.opts.ClaimInterval != 2*time.Second {
		t.Errorf("explicit options were overwritten: %+v", b.opts)
	}
}
