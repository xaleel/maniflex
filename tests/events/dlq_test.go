package events_test

// 11D.10 — what a dead-lettered event carries.
//
// There are two DLQ paths and they did not agree. events.DeliverWithRetry mints
// a fresh ID and stamps original_type + original_id; outbox.Relayer.routeToDLQ
// stamped only original_type, reused the original ID, and discarded the publish
// error. Both copy the original headers through, which is what this row asked
// for and was already true — the divergence is the real defect.
//
// Reusing the ID is the one that bites: DeliverWithRetry's own comment says the
// fresh ID exists "so downstream dedupers see this as a new event". The outbox
// path publishes a DLQ event under the ID of the event that was already
// published, so any deduper drops it — the failure is dead-lettered into
// nothing.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xaleel/maniflex/events"
)

// capturePub records everything published, and can be made to fail.
type capturePub struct {
	mu   sync.Mutex
	got  []events.Event
	fail error
}

func (p *capturePub) Publish(_ context.Context, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.got = append(p.got, e)
	return nil
}

func (p *capturePub) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *capturePub) Close() error { return nil }

func (p *capturePub) published() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.got...)
}

// origEvent is the event both paths dead-letter, carrying an app header that
// must survive the trip.
func origEvent() events.Event {
	return events.Event{
		ID:      "evt-original",
		Type:    "order.created",
		Source:  "orders",
		Headers: map[string]string{"tenant": "acme", "trace": "abc123"},
	}
}

// assertDLQContract is the shape both paths must produce.
func assertDLQContract(t *testing.T, who string, orig, dlq events.Event, wantType string) {
	t.Helper()
	if dlq.Type != wantType {
		t.Errorf("%s: Type = %q, want %q", who, dlq.Type, wantType)
	}
	// A fresh ID: the original was already published under its own ID, so
	// reusing it means a deduper treats the dead-letter as a duplicate and
	// drops it.
	if dlq.ID == orig.ID {
		t.Errorf("%s: DLQ event reuses the original ID %q — a downstream deduper "+
			"will drop it as a duplicate", who, orig.ID)
	}
	if dlq.ID == "" {
		t.Errorf("%s: DLQ event has no ID", who)
	}
	// The original headers travel with it — this row's actual ask.
	for k, v := range orig.Headers {
		if dlq.Headers[k] != v {
			t.Errorf("%s: original header %q = %q, want %q",
				who, k, dlq.Headers[k], v)
		}
	}
	// ...and enough provenance to find the original.
	if dlq.Headers["original_type"] != orig.Type {
		t.Errorf("%s: original_type = %q, want %q",
			who, dlq.Headers["original_type"], orig.Type)
	}
	if dlq.Headers["original_id"] != orig.ID {
		t.Errorf("%s: original_id = %q, want %q — without it the dead-letter "+
			"cannot be traced back to the event it came from",
			who, dlq.Headers["original_id"], orig.ID)
	}
}

func TestDLQ_DeliverWithRetryPayload(t *testing.T) {
	pub := &capturePub{}
	orig := origEvent()

	events.DeliverWithRetry(context.Background(), pub, events.Subscription{
		Patterns: []string{orig.Type},
		MaxRetry: 0,
		DLQ:      "order.created.dlq",
		Handler: func(context.Context, events.Event) error {
			return errors.New("handler boom")
		},
	}, orig)

	got := pub.published()
	if len(got) != 1 {
		t.Fatalf("expected 1 DLQ publish, got %d", len(got))
	}
	assertDLQContract(t, "DeliverWithRetry", orig, got[0], "order.created.dlq")
}

// TestDLQ_OriginalIsNotMutated: the DLQ event is derived from the original, and
// headers are a reference type — building it by assigning into the original's
// map would corrupt the event the caller still holds.
func TestDLQ_OriginalIsNotMutated(t *testing.T) {
	pub := &capturePub{}
	orig := origEvent()

	events.DeliverWithRetry(context.Background(), pub, events.Subscription{
		Patterns: []string{orig.Type}, MaxRetry: 0, DLQ: "d",
		Handler: func(context.Context, events.Event) error { return errors.New("x") },
	}, orig)

	if _, leaked := orig.Headers["original_type"]; leaked {
		t.Error("dead-lettering wrote DLQ metadata into the original event's headers")
	}
	if orig.ID != "evt-original" || orig.Type != "order.created" {
		t.Errorf("original event was mutated: %+v", orig)
	}
}
