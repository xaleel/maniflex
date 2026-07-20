package redis

// Audit EV-13: a failed XREADGROUP retried after a bare time.Sleep(time.Second)
// — fixed interval, no jitter, and blind to the context.
//
// backoff_test.go covers the policy itself (events.ReadBackoff). These tests
// cover the wiring, which is where the mistakes actually live: that the loop
// calls the backoff instead of a fixed sleep, that an idle stream does not
// count as a failure, that a recovered stream starts over, and that shutdown
// no longer waits out the delay. The streamOps seam added in EV-7 makes all of
// that reachable with no Redis server present.
//
//	go test ./events/redis/... -run TestReadBackoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/events"
)

var errBrokerDown = errors.New("dial tcp 127.0.0.1:6379: connection refused")

// failingOps fails every read, recording when each attempt arrived so the
// spacing between them can be asserted.
type failingOps struct {
	mu    sync.Mutex
	times []time.Time
	// afterN reads succeed (returning nothing) once this many failures have
	// been served; zero means never recover.
	afterN int
}

func (f *failingOps) EnsureGroup(context.Context, string, string) error { return nil }

func (f *failingOps) ReadGroup(_ context.Context, _, _, _ string, _ int64, _ time.Duration) ([]goredis.XStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.times = append(f.times, time.Now())
	if f.afterN > 0 && len(f.times) > f.afterN {
		return nil, nil
	}
	return nil, errBrokerDown
}

func (f *failingOps) AutoClaim(context.Context, string, string, string, time.Duration, string, int64) ([]goredis.XMessage, string, error) {
	return nil, "0-0", nil
}
func (f *failingOps) Ack(context.Context, string, string, string) error { return nil }

func (f *failingOps) attempts() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.times...)
}

// The core regression. With the old fixed sleep, a broker down for 700ms of
// wall clock produced a read attempt every second — and critically, the gaps
// never widened. Here each gap must be strictly larger than the last.
func TestReadBackoff_GapsWidenWhileTheBrokerIsDown(t *testing.T) {
	ops := &failingOps{}
	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 1200*time.Millisecond)

	at := ops.attempts()
	if len(at) < 4 {
		t.Fatalf("only %d read attempts in 1.2s; expected several while failing", len(at))
	}
	if len(at) > 30 {
		t.Errorf("%d read attempts in 1.2s: the loop is hammering a broker that is down", len(at))
	}

	// Compare the first gap against a much later one rather than every
	// consecutive pair: jitter means an individual gap can be shorter than its
	// predecessor, which is the point of jitter.
	first := at[1].Sub(at[0])
	last := at[len(at)-1].Sub(at[len(at)-2])
	if last <= first {
		t.Errorf("gap did not widen: first %v, last %v — this is the fixed-interval bug", first, last)
	}
}

// idleThenFailingOps returns goredis.Nil — the block timeout expiring on a
// healthy but quiet stream — for the first idleReads calls, then fails.
type idleThenFailingOps struct {
	mu        sync.Mutex
	idleReads int
	times     []time.Time
}

func (f *idleThenFailingOps) EnsureGroup(context.Context, string, string) error { return nil }

func (f *idleThenFailingOps) ReadGroup(_ context.Context, _, _, _ string, _ int64, _ time.Duration) ([]goredis.XStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.times = append(f.times, time.Now())
	if len(f.times) <= f.idleReads {
		return nil, goredis.Nil
	}
	return nil, errBrokerDown
}

func (f *idleThenFailingOps) AutoClaim(context.Context, string, string, string, time.Duration, string, int64) ([]goredis.XMessage, string, error) {
	return nil, "0-0", nil
}
func (f *idleThenFailingOps) Ack(context.Context, string, string, string) error { return nil }

// goredis.Nil is the idle steady state, not a failure. If the loop counted it
// as one, a consumer sitting on a quiet stream would climb to the 30s ceiling
// within seconds and then take half a minute to notice its first real message.
//
// Attempt numbers are not observable from outside, so this asserts on what
// they cause: after a long idle run, the first genuine error must be followed
// by a short first-attempt delay, not by a ceiling-length one.
func TestReadBackoff_IdleStreamDoesNotAdvanceTheBackoff(t *testing.T) {
	ops := &idleThenFailingOps{idleReads: 12}
	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 700*time.Millisecond)

	ops.mu.Lock()
	total := len(ops.times)
	ops.mu.Unlock()

	// 12 idle reads cost nothing, so they complete near-instantly and leave the
	// bulk of the window for real retries. Had they advanced the backoff, read
	// 13 would be followed by a ~30s wait and the count would stop at 13.
	if total <= ops.idleReads+2 {
		t.Errorf("only %d reads, want more than %d: an idle stream drove the backoff to its ceiling",
			total, ops.idleReads+2)
	}
}

// After a recovery the next failure must start from Min again. Without Reset,
// a consumer that hit the ceiling once would stay near a 30s retry for the
// rest of the process lifetime, even through hours of healthy operation.
func TestReadBackoff_ResetsAfterASuccessfulRead(t *testing.T) {
	ops := &failingOps{afterN: 2}
	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 600*time.Millisecond)

	at := ops.attempts()
	if len(at) < 6 {
		t.Fatalf("only %d attempts; the loop stalled after recovering", len(at))
	}

	// Reads 3+ succeed, so they should follow one another with no backoff at
	// all. If Reset were missing the loop would still be honouring the delay
	// accumulated during the two failures.
	tail := at[len(at)-1].Sub(at[len(at)-3])
	if tail > 100*time.Millisecond {
		t.Errorf("successful reads are %v apart: the backoff was not reset after recovery", tail)
	}
}

// The reason time.Sleep had to go. With a fixed sleep the loop noticed
// cancellation only after the current second elapsed; with a growing backoff
// that would have become a 30s stall on every shutdown during an outage.
func TestReadBackoff_CancelDuringBackoffReturnsPromptly(t *testing.T) {
	// A long floor guarantees the consumer is parked inside the backoff wait,
	// not between reads, when cancel arrives.
	b := busWith(&failingOps{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.runConsumer(ctx, "test:stream", subWith([]string{"*"}, noopHandler))
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	start := time.Now()
	select {
	case <-done:
		// Well under the 1s the old fixed sleep would have cost: a ctx-aware
		// wait returns essentially instantly, so anything approaching a full
		// sleep interval means the delay is being waited out.
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Errorf("consumer took %v to stop: shutdown is waiting out the backoff", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer never returned after cancel; the backoff is ignoring ctx")
	}
}

// Anti-vacuity: the tests above would all pass against a loop that gave up on
// the first error. A read loop must never stop retrying — a consumer that
// quits looks identical to one with nothing to consume.
func TestReadBackoff_LoopKeepsRetryingForever(t *testing.T) {
	ops := &failingOps{}
	runFor(t, busWith(ops), subWith([]string{"*"}, noopHandler), 800*time.Millisecond)

	if n := len(ops.attempts()); n < 3 {
		t.Errorf("only %d attempts: the loop gave up instead of retrying", n)
	}
}

// The helper is shared across modules, so a change to its defaults silently
// changes both adapters. Pin the contract the loops depend on.
func TestReadBackoff_DefaultsAreSaneForAReadLoop(t *testing.T) {
	if events.DefaultReadBackoffMin > time.Second {
		t.Errorf("Min of %v makes a one-off blip expensive", events.DefaultReadBackoffMin)
	}
	if events.DefaultReadBackoffMax > time.Minute {
		t.Errorf("Max of %v leaves a recovered broker unnoticed for too long", events.DefaultReadBackoffMax)
	}
}
