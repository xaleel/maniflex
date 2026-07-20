package redis

// Audit EV-14: every event is written to two streams — its own typed stream and
// the hub that wildcard subscribers read — and the pair went out on a plain
// Pipeline. A pipeline batches; it is not a transaction. A connection lost
// between the two writes left the event on one stream and not the other, and
// because nothing reconciles them afterwards the two kinds of subscriber
// disagreed permanently about whether the event happened.
//
// The same call sites hard-coded `MaxLen: 100_000, Approx: true`. Trimming
// deletes the oldest entries without consulting consumer groups, so an entry
// still pending in a group's PEL is dropped with the rest — no error to the
// publisher, no notice to the consumer. That was neither configurable nor
// documented.
//
// A third defect the audit does not name: PublishBatch discarded the result of
// json.Marshal, publishing an empty payload no subscriber can decode. EV-8
// established that an undecodable payload is a poison entry; this manufactured
// one out of an error that was right there to be returned.
//
// There is no Redis server here, and none is needed. go-redis's TxPipeline
// wraps its commands in MULTI/EXEC before handing them to the pipeline hook,
// so a hook that records and short-circuits sees the exact command sequence
// that would go on the wire — including whether it is a transaction.
//
//	go test ./events/redis/... -run TestPublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xaleel/maniflex/events"
)

// errIntercepted stops execution inside the hook, before go-redis takes a
// connection from the pool — so nothing dials and no server is needed.
var errIntercepted = errors.New("intercepted")

// recordingHook captures the commands of every pipeline execution and prevents
// it from reaching a socket.
type recordingHook struct {
	mu    sync.Mutex
	execs [][]goredis.Cmder
}

func (h *recordingHook) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *recordingHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }

func (h *recordingHook) ProcessPipelineHook(_ goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(_ context.Context, cmds []goredis.Cmder) error {
		h.mu.Lock()
		h.execs = append(h.execs, cmds)
		h.mu.Unlock()
		return errIntercepted
	}
}

// taken returns the recorded executions and clears them.
func (h *recordingHook) taken() [][]goredis.Cmder {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([][]goredis.Cmder(nil), h.execs...)
	h.execs = nil
	return out
}

func newProbedBus(t *testing.T, opts ...Options) (*Bus, *recordingHook) {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = c.Close() })

	h := &recordingHook{}
	c.AddHook(h)
	return New(c, "app", opts...), h
}

// names renders the command verbs of one execution, lowercased.
func names(cmds []goredis.Cmder) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.ToLower(c.Name())
	}
	return out
}

// argsOf renders one command's full argument list as lowercase strings, which
// is what actually goes on the wire.
func argsOf(c goredis.Cmder) []string {
	raw := c.Args()
	out := make([]string, len(raw))
	for i, a := range raw {
		if s, ok := a.(string); ok {
			out[i] = strings.ToLower(s)
			continue
		}
		out[i] = strings.ToLower(fmt.Sprint(a))
	}
	return out
}

// xadds returns the XADD commands of a transactional execution, failing the
// test unless the execution is in fact wrapped in MULTI/EXEC.
//
// Every assertion about arguments routes through here, so none of them can
// pass against a plain pipeline.
func xadds(t *testing.T, cmds []goredis.Cmder) []goredis.Cmder {
	t.Helper()
	got := names(cmds)
	if len(cmds) < 2 || got[0] != "multi" || got[len(got)-1] != "exec" {
		t.Fatalf("command sequence %v is not wrapped in MULTI/EXEC: "+
			"a connection lost mid-publish writes one stream and not the other", got)
	}
	return cmds[1 : len(cmds)-1]
}

func sampleEvent() events.Event {
	return events.Event{ID: "evt-1", Type: "invoice.created", Subject: "inv-9"}
}

func oneExec(t *testing.T, h *recordingHook) []goredis.Cmder {
	t.Helper()
	execs := h.taken()
	if len(execs) != 1 {
		t.Fatalf("%d pipeline executions, want 1", len(execs))
	}
	return execs[0]
}

// The EV-14 regression: both XADDs must ride one MULTI/EXEC.
func TestPublish_PairIsOneTransaction(t *testing.T) {
	b, h := newProbedBus(t)

	if err := b.Publish(context.Background(), sampleEvent()); !errors.Is(err, errIntercepted) {
		t.Fatalf("Publish error = %v, want the interception sentinel", err)
	}

	adds := xadds(t, oneExec(t, h))
	if len(adds) != 2 {
		t.Fatalf("%d commands inside the transaction, want 2 (typed + hub)", len(adds))
	}
	for _, c := range adds {
		if strings.ToLower(c.Name()) != "xadd" {
			t.Errorf("command %q inside the transaction, want xadd", c.Name())
		}
	}
}

// An atomic write to the wrong pair of keys is still wrong.
func TestPublish_WritesTypedAndHubStreams(t *testing.T) {
	b, h := newProbedBus(t)
	_ = b.Publish(context.Background(), sampleEvent())

	var streams []string
	for _, c := range xadds(t, oneExec(t, h)) {
		streams = append(streams, argsOf(c)[1])
	}

	want := []string{"app:invoice.created", "app:*"}
	if len(streams) != 2 || streams[0] != want[0] || streams[1] != want[1] {
		t.Errorf("streams = %v, want %v", streams, want)
	}
}

// The default must not change for existing deployments: 100k, approximate.
func TestPublish_DefaultTrimIsUnchanged(t *testing.T) {
	b, h := newProbedBus(t)
	_ = b.Publish(context.Background(), sampleEvent())

	a := argsOf(xadds(t, oneExec(t, h))[0])
	if !hasSeq(a, "maxlen", "~", "100000") {
		t.Errorf("args = %v, want an approximate MAXLEN of 100000", a)
	}
}

func TestPublish_MaxLenIsConfigurable(t *testing.T) {
	b, h := newProbedBus(t, Options{MaxLen: 500})
	_ = b.Publish(context.Background(), sampleEvent())

	a := argsOf(xadds(t, oneExec(t, h))[0])
	if !hasSeq(a, "maxlen", "~", "500") {
		t.Errorf("args = %v, want MAXLEN 500", a)
	}
}

// The point of the option: an operator who would rather exhaust memory than
// silently drop an unacknowledged event must be able to say so.
func TestPublish_MaxLenUnlimitedEmitsNoTrim(t *testing.T) {
	b, h := newProbedBus(t, Options{MaxLen: MaxLenUnlimited})
	_ = b.Publish(context.Background(), sampleEvent())

	a := argsOf(xadds(t, oneExec(t, h))[0])
	for _, tok := range a {
		if tok == "maxlen" {
			t.Fatalf("args = %v, want no MAXLEN clause: the stream would still be trimmed", a)
		}
	}
}

// Zero means "unset", and unset must take the default rather than being read
// as a request for no trimming — the two are one keystroke apart.
func TestPublish_ZeroMaxLenTakesTheDefault(t *testing.T) {
	b, h := newProbedBus(t, Options{MaxLen: 0})
	_ = b.Publish(context.Background(), sampleEvent())

	a := argsOf(xadds(t, oneExec(t, h))[0])
	if !hasSeq(a, "maxlen", "~", "100000") {
		t.Errorf("args = %v, want the 100000 default when MaxLen is unset", a)
	}
}

func TestPublish_BatchIsOneTransaction(t *testing.T) {
	b, h := newProbedBus(t)

	es := []events.Event{
		{ID: "a", Type: "invoice.created"},
		{ID: "b", Type: "invoice.paid"},
	}
	if err := b.PublishBatch(context.Background(), es); !errors.Is(err, errIntercepted) {
		t.Fatalf("PublishBatch error = %v, want the interception sentinel", err)
	}

	if n := len(xadds(t, oneExec(t, h))); n != 4 {
		t.Errorf("%d commands, want 4 (two events × typed + hub)", n)
	}
}

// The third defect. A payload that will not marshal must be reported, not
// published as an empty string that every subscriber then fails to decode.
func TestPublish_BatchReportsMarshalFailure(t *testing.T) {
	b, h := newProbedBus(t)

	es := []events.Event{
		{ID: "good", Type: "invoice.created"},
		{ID: "bad", Type: "invoice.paid", Data: json.RawMessage("{not json")},
	}
	err := b.PublishBatch(context.Background(), es)

	if err == nil || errors.Is(err, errIntercepted) {
		t.Fatalf("PublishBatch error = %v, want a marshal failure", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error %q does not name the offending event", err)
	}
	if n := len(h.taken()); n != 0 {
		t.Errorf("%d pipeline executions after a marshal failure, want 0: "+
			"the batch must not go out half-built", n)
	}
}

// Publish already checked this; a regression test so the two paths cannot
// drift apart again.
func TestPublish_SingleReportsMarshalFailure(t *testing.T) {
	b, h := newProbedBus(t)

	err := b.Publish(context.Background(), events.Event{
		ID: "bad", Type: "invoice.paid", Data: json.RawMessage("{not json"),
	})
	if err == nil || errors.Is(err, errIntercepted) {
		t.Fatalf("Publish error = %v, want a marshal failure", err)
	}
	if n := len(h.taken()); n != 0 {
		t.Errorf("%d pipeline executions after a marshal failure, want 0", n)
	}
}

// Anti-vacuity: an empty batch must not open a transaction at all.
func TestPublish_EmptyBatchDoesNothing(t *testing.T) {
	b, h := newProbedBus(t)

	if err := b.PublishBatch(context.Background(), nil); err != nil {
		t.Errorf("empty batch returned %v, want nil", err)
	}
	if n := len(h.taken()); n != 0 {
		t.Errorf("%d executions for an empty batch, want 0", n)
	}
}

// Anti-vacuity for xadds itself: it must actually reject a plain pipeline, or
// every assertion routed through it proves nothing.
func TestPublish_TransactionCheckRejectsAPlainPipeline(t *testing.T) {
	c := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:0"})
	defer c.Close()
	h := &recordingHook{}
	c.AddHook(h)

	p := c.Pipeline()
	p.XAdd(context.Background(), &goredis.XAddArgs{Stream: "s", Values: map[string]any{"k": "v"}})
	_, _ = p.Exec(context.Background())

	cmds := oneExec(t, h)
	got := names(cmds)
	if len(got) > 0 && (got[0] == "multi" || got[len(got)-1] == "exec") {
		t.Fatalf("a plain pipeline produced %v, which xadds would accept as a transaction", got)
	}
}

// hasSeq reports whether toks appear consecutively in args.
func hasSeq(args []string, toks ...string) bool {
	for i := 0; i+len(toks) <= len(args); i++ {
		ok := true
		for j, tok := range toks {
			if args[i+j] != tok {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
